package config

import "testing"

func TestVerifyHostFatalOnContradiction(t *testing.T) {
	_, err := VerifyHost("codex", HostEvidence{Detected: "claude", Reason: "CLAUDECODE is set"})
	if err == nil {
		t.Fatal("a declared host contradicted by the environment must be fatal")
	}
}

func TestVerifyHostWarnsOnUnknown(t *testing.T) {
	warn, err := VerifyHost("codex", HostEvidence{})
	if err != nil {
		t.Fatalf("absence of evidence must not be fatal: %v", err)
	}
	if warn == "" {
		t.Fatal("expected a warning when the host cannot be confirmed")
	}
}

func TestVerifyHostSilentOnAgreement(t *testing.T) {
	warn, err := VerifyHost("claude", HostEvidence{Detected: "claude", Reason: "CLAUDECODE is set"})
	if err != nil || warn != "" {
		t.Fatalf("agreement must be silent: warn=%q err=%v", warn, err)
	}
}

func TestVerifyHostRequiresDeclaration(t *testing.T) {
	if _, err := VerifyHost("", HostEvidence{}); err == nil {
		t.Fatal("--host must be required")
	}
}

func TestDetectHostReadsEnvironment(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	if ev := DetectHost(); ev.Detected != "claude" {
		t.Fatalf("Detected = %q, want claude", ev.Detected)
	}
}

func TestVerifyHostFatalWhenBothAgreeButEnvironmentDisagrees(t *testing.T) {
	// The dangerous copy-paste: a codex MCP block pasted into Claude Code. The
	// declaration is self-consistent and still wrong, so only the environment
	// can catch it.
	t.Setenv("CLAUDECODE", "1")
	if _, err := VerifyHost("codex", DetectHost()); err == nil {
		t.Fatal("a config block copied into the wrong agent must be refused")
	}
}
