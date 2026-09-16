//go:build windows

package platform

import (
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestPipeNameIsStableAndNamespaced pins the property the endpoint and the
// dialler both depend on: the same endpoint path maps to the same pipe name in
// both processes, or the client would dial a name the server never created.
func TestPipeNameIsStableAndNamespaced(t *testing.T) {
	const endpoint = `C:\Users\op\AppData\Local\agents-bridge\runtime\1234.sock`
	first, err := pipeName(endpoint)
	if err != nil {
		t.Fatalf("pipeName: %v", err)
	}
	second, err := pipeName(endpoint)
	if err != nil {
		t.Fatalf("pipeName (second call): %v", err)
	}
	if first != second {
		t.Fatalf("pipeName is not deterministic: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, pipePrefix) {
		t.Fatalf("pipeName(%q) = %q, want the %q prefix", endpoint, first, pipePrefix)
	}
	// The readable tail helps an operator recognise a live endpoint.
	if !strings.Contains(first, "1234.sock") {
		t.Fatalf("pipeName(%q) = %q, want the endpoint's base name to survive", endpoint, first)
	}
}

// TestPipeNameIsCaseInsensitive pins that two spellings of one path do not
// become two endpoints, because the filesystem the paths come from ignores case.
func TestPipeNameIsCaseInsensitive(t *testing.T) {
	lower, err := pipeName(`C:\runtime\1234.sock`)
	if err != nil {
		t.Fatalf("pipeName: %v", err)
	}
	upper, err := pipeName(`C:\RUNTIME\1234.SOCK`)
	if err != nil {
		t.Fatalf("pipeName: %v", err)
	}
	if lower != upper {
		t.Fatalf("case variants map to different pipes: %q vs %q", lower, upper)
	}
}

// TestPipeNameSeparatesDistinctEndpoints pins that two endpoints sharing a base
// name stay distinct. The operator control socket and a gate socket can both end
// in the same file name in different directories; collapsing them would point a
// delegated agent's gate client at the operator's control endpoint.
func TestPipeNameSeparatesDistinctEndpoints(t *testing.T) {
	a, err := pipeName(`C:\runtime\gate\gate-run-1.sock`)
	if err != nil {
		t.Fatalf("pipeName: %v", err)
	}
	b, err := pipeName(`C:\other\gate\gate-run-1.sock`)
	if err != nil {
		t.Fatalf("pipeName: %v", err)
	}
	if a == b {
		t.Fatalf("distinct endpoints collapsed onto one pipe name: %q", a)
	}
}

// TestPipeNameRejectsEmpty pins that an unnamed endpoint is an error rather
// than a pipe at the bare prefix, which every bridge on the machine would share.
func TestPipeNameRejectsEmpty(t *testing.T) {
	if got, err := pipeName(""); err == nil {
		t.Fatalf("pipeName(\"\") = %q, want an error", got)
	}
}

// TestSanitisePipeComponentDropsNamespaceCharacters pins that no caller-derived
// text can extend the pipe path or leave the agents-bridge namespace.
func TestSanitisePipeComponentDropsNamespaceCharacters(t *testing.T) {
	for _, in := range []string{`..\..\evil`, `a\b`, `a/b`, `a:b`, `a"b`} {
		got := sanitisePipeComponent(in)
		for _, bad := range []string{`\`, `/`, `:`, `"`, `..`} {
			if strings.Contains(got, bad) {
				t.Fatalf("sanitisePipeComponent(%q) = %q, still contains %q", in, got, bad)
			}
		}
	}
	if got := sanitisePipeComponent(""); got == "" {
		t.Fatal("sanitisePipeComponent(\"\") must not return an empty component")
	}
	if got := sanitisePipeComponent(strings.Repeat("a", 500)); len(got) > 64 {
		t.Fatalf("sanitisePipeComponent did not cap the tail: %d characters", len(got))
	}
}

// TestOwnerOnlySDDLNamesThisAccountAndIsProtected pins the DACL the control
// pipe and the state directories are created with. The ACE must carry this
// process's own SID: a hardcoded well-known SID such as Users or Everyone would
// grant the wrong set on a shared or domain-joined machine, which is the whole
// of docs/03-threat-model.md T10.
func TestOwnerOnlySDDLNamesThisAccountAndIsProtected(t *testing.T) {
	sid, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	sddl := ownerOnlySDDL(sid)

	if !strings.HasPrefix(sddl, "D:P(") {
		t.Fatalf("SDDL %q is not a protected DACL; inherited ACEs could widen it", sddl)
	}
	if !strings.Contains(sddl, sid.String()) {
		t.Fatalf("SDDL %q does not name this account's SID %s", sddl, sid.String())
	}
	// "WD" is Everyone and "BU" is Builtin Users in SDDL shorthand.
	for _, forbidden := range []string{";WD)", ";BU)", ";AU)"} {
		if strings.Contains(sddl, forbidden) {
			t.Fatalf("SDDL %q grants a well-known group %q", sddl, forbidden)
		}
	}
	// It must parse: a descriptor the API rejects would fail only at runtime.
	if _, err := windows.SecurityDescriptorFromString(sddl); err != nil {
		t.Fatalf("SecurityDescriptorFromString(%q): %v", sddl, err)
	}
}

// TestOwnerOnlySecurityAttributesAreUsable pins that the SECURITY_ATTRIBUTES
// handed to CreateNamedPipe and CreateDirectory are fully formed: a zero Length
// or a nil descriptor would be accepted by the compiler and produce a default,
// world-readable object at runtime.
func TestOwnerOnlySecurityAttributesAreUsable(t *testing.T) {
	sa, sd, err := ownerOnlySecurityAttributes()
	if err != nil {
		t.Fatalf("ownerOnlySecurityAttributes: %v", err)
	}
	if sa.Length == 0 {
		t.Fatal("SECURITY_ATTRIBUTES.Length is zero; the DACL would be ignored")
	}
	if sa.SecurityDescriptor == nil || sd == nil {
		t.Fatal("SECURITY_ATTRIBUTES carries no security descriptor")
	}
	if sa.InheritHandle != 0 {
		t.Fatal("the control endpoint's handle must not be inheritable")
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL: %v", err)
	}
	if defaulted || dacl == nil {
		t.Fatal("descriptor has no explicit DACL; the object would inherit a default one")
	}
}

// TestVerifyPeerRejectsAForeignConnectionType pins that VerifyPeer denies
// anything that is not one of our own pipe connections. A caller passing some
// other net.Conn must not be waved through for lack of a check.
func TestVerifyPeerRejectsAForeignConnectionType(t *testing.T) {
	if err := NewControlEndpoint().VerifyPeer(nil); err == nil {
		t.Fatal("VerifyPeer(nil) returned no error")
	}
}

// TestAttachRefusesAfterStart pins the ordering the ProcessGroup contract
// requires: a job assigned after the child is running races it spawning
// grandchildren that the job would never capture.
func TestAttachRefusesAfterStart(t *testing.T) {
	g := NewProcessGroup()
	cmd := exec.Command("cmd.exe", "/c", "exit")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a probe process: %v", err)
	}
	defer func() { _ = cmd.Wait() }()
	if err := g.Attach(cmd); err == nil {
		t.Fatal("Attach after Start was accepted")
	}
}

// TestAttachSetsSuspendedCreationFlags pins that the child is created suspended,
// which is what makes the job assignment in AfterStart race-free.
func TestAttachSetsSuspendedCreationFlags(t *testing.T) {
	g := NewProcessGroup()
	cmd := exec.Command("cmd.exe", "/c", "exit")
	if err := g.Attach(cmd); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() { _ = g.KillAll() }()

	if cmd.SysProcAttr == nil {
		t.Fatal("Attach set no SysProcAttr")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED == 0 {
		t.Fatal("child is not created suspended; the job assignment would race it")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("child is not detached from the bridge's console group")
	}
}

// TestKillAllIsIdempotent pins that reaping twice is not an error: the run
// supervisor calls KillAll on several paths, including after the process has
// already exited.
func TestKillAllIsIdempotent(t *testing.T) {
	g := NewProcessGroup()
	if err := g.KillAll(); err != nil {
		t.Fatalf("KillAll before Attach: %v", err)
	}
	cmd := exec.Command("cmd.exe", "/c", "exit")
	if err := g.Attach(cmd); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := g.KillAll(); err != nil {
		t.Fatalf("first KillAll: %v", err)
	}
	if err := g.KillAll(); err != nil {
		t.Fatalf("second KillAll: %v", err)
	}
}

// TestWindowsGroupImplementsPostStarter pins the seam the run supervisor needs:
// without AfterStart the child stays suspended forever.
func TestWindowsGroupImplementsPostStarter(t *testing.T) {
	if _, ok := NewProcessGroup().(PostStarter); !ok {
		t.Fatal("the Windows process group does not implement PostStarter")
	}
}

// TestAfterStartRefusesBeforeTheProcessExists pins that the binding cannot be
// completed against a Cmd that was never started.
func TestAfterStartRefusesBeforeTheProcessExists(t *testing.T) {
	g := NewProcessGroup()
	ps, ok := g.(PostStarter)
	if !ok {
		t.Fatal("not a PostStarter")
	}
	if err := ps.AfterStart(); err == nil {
		t.Fatal("AfterStart without Attach was accepted")
	}
	cmd := exec.Command("cmd.exe", "/c", "exit")
	if err := g.Attach(cmd); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() { _ = g.KillAll() }()
	if err := ps.AfterStart(); err == nil {
		t.Fatal("AfterStart before Start was accepted")
	}
}
