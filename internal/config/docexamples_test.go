package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocumentedExamplesLoad keeps the specification and the implementation from
// drifting: every complete YAML example in docs/04-config-schema.md must survive
// the real loader, semantic rules included. A doc example that would be rejected
// at runtime is a documentation bug, and this is the test that finds it.
func TestDocumentedExamplesLoad(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "docs", "04-config-schema.md"))
	if err != nil {
		t.Skipf("config schema doc unavailable: %v", err)
	}
	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(src), -1)
	if len(blocks) == 0 {
		t.Fatal("no yaml examples found in docs/04-config-schema.md")
	}

	checked := 0
	for i, b := range blocks {
		body := b[1]
		if !strings.Contains(body, "version:") || !strings.Contains(body, "agents:") {
			continue // a fragment, not a complete config
		}
		checked++
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		// Commands in the docs are real vendor CLIs that need not be installed
		// here, so PATH resolution is stubbed; every other rule runs for real.
		cfg, err := Load(path, Options{SkipPermissionCheck: true, LookPath: fakeLookPath(t)})
		if err != nil {
			t.Errorf("docs/04 yaml block %d does not load: %v", i, err)
			continue
		}
		assertGateFlagsNeverPrecedeSandboxFlags(t, cfg, fmt.Sprintf("docs/04 yaml block %d", i))
	}
	if checked == 0 {
		t.Fatal("no complete config examples were checked")
	}
	t.Logf("%d documented config examples load cleanly", checked)
}

// TestExampleConfigGateFlagOrder loads the shipped examples/config.yaml
// through the real loader and checks the one property that keeps the
// interactive gate fail-closed by default: wherever an adapter's invoke
// template uses both {{sandbox_flags}} and {{gate_flags}}, the sandbox
// placeholder comes first. Last flag wins on the actual argv (VERIFIED
// against claude 2.1.272, docs/12-spike-results.md C8), so this order is what
// lets a gate flag such as --permission-prompts host override the sandbox
// list's fail-closed --permission-prompts none only when the gate flags are
// actually emitted, never the reverse.
//
// This is an argv-shape assertion on the shipped config, not a loader-level
// rule: encoding "which placeholder must come first" as a load error would
// require the loader to know that --permission-prompts specifically is
// order-sensitive, i.e. hard-code vendor flag knowledge in Go, which
// contradicts the same principle rule 18's now-removed second half violated.
// Vendor knowledge stays in adapter YAML and the stream parser.
func TestExampleConfigGateFlagOrder(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "examples", "config.yaml")
	cfg, err := Load(path, Options{SkipPermissionCheck: true, LookPath: fakeLookPath(t)})
	if err != nil {
		t.Fatalf("examples/config.yaml does not load: %v", err)
	}
	assertGateFlagsNeverPrecedeSandboxFlags(t, cfg, "examples/config.yaml")
}

func assertGateFlagsNeverPrecedeSandboxFlags(t *testing.T, cfg *Config, source string) {
	t.Helper()
	for id, a := range cfg.Agents {
		for _, inv := range []struct {
			name string
			v    *Invocation
		}{{"invoke", a.Invoke}, {"resume_invoke", a.ResumeInvoke}} {
			if inv.v == nil {
				continue
			}
			sandboxAt, gateAt := -1, -1
			for i, arg := range inv.v.Args {
				switch arg {
				case "{{sandbox_flags}}":
					sandboxAt = i
				case "{{gate_flags}}":
					gateAt = i
				}
			}
			if sandboxAt >= 0 && gateAt >= 0 && gateAt < sandboxAt {
				t.Errorf("%s: agents.%s.%s places {{gate_flags}} before {{sandbox_flags}}; "+
					"since last flag wins on the actual argv, that would make a sandbox flag "+
					"override an interactive gate flag instead of the other way around", source, id, inv.name)
			}
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repository root not found")
	return ""
}
