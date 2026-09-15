package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/config"
	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// probeCommand checks an adapter's binary actually runs, without invoking a
// model. A missing or broken CLI should be a doctor finding, not a failed
// delegation later.
func probeCommand(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s --version failed: %v: %s", path, err, firstLine(string(out)))
	}
	return nil
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// runDoctor reports every fault it can find rather than stopping at the first,
// so one run tells the operator everything that needs fixing.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	host := fs.String("host", "", "agent_id to verify against the environment")
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	faults := 0
	report := func(ok bool, format string, a ...any) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			faults++
		}
		fmt.Printf("%s  %s\n", mark, fmt.Sprintf(format, a...))
	}

	paths, err := platform.NewPaths()
	if err != nil {
		report(false, "paths: %v", err)
		return fmt.Errorf("%d fault(s)", faults)
	}
	report(true, "config path   %s", paths.Config())
	report(true, "state dir     %s", paths.State())
	report(true, "runtime dir   %s", paths.Runtime())

	for _, d := range []string{paths.State(), paths.Runtime()} {
		fi, err := os.Stat(d)
		switch {
		case err != nil:
			report(false, "%s: %v", d, err)
		case fi.Mode().Perm() != 0o700:
			report(false, "%s is mode %04o, want 0700", d, fi.Mode().Perm())
		default:
			report(true, "%s is owner-only", d)
		}
	}

	ev := config.DetectHost()
	if ev.Detected == "" {
		report(true, "host detection: no signal (normal outside a host agent)")
	} else {
		report(true, "host detection: %s (%s)", ev.Detected, ev.Reason)
	}
	if *host != "" {
		if _, err := config.VerifyHost(*host, ev); err != nil {
			report(false, "host check: %v", err)
		} else {
			report(true, "host check: --host %s agrees with the environment", *host)
		}
	}

	if *configPath == "" {
		*configPath = paths.Config()
	}
	if _, err := os.Stat(*configPath); err != nil {
		report(false, "config: %v", err)
	} else {
		cfg, err := config.Load(*configPath, config.Options{Host: *host})
		if err != nil {
			report(false, "config: %v", err)
		} else {
			report(true, "config loads; %d delegation target(s) after self-exclusion", len(cfg.Agents))

			for _, root := range cfg.AllowedRoots {
				canonical, err := platform.NewPathGuard().Canonicalise(root)
				if err != nil {
					report(false, "allowed root %s: %v", root, err)
					continue
				}
				fi, err := os.Stat(canonical)
				switch {
				case err != nil:
					report(false, "allowed root %s: %v", canonical, err)
				case !fi.IsDir():
					report(false, "allowed root %s is not a directory", canonical)
				default:
					report(true, "allowed root %s", canonical)
				}
			}

			ids := make([]string, 0, len(cfg.Agents))
			for id := range cfg.Agents {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				a := cfg.Agents[id]
				report(true, "adapter %s (%s, %s) -> %s", id, a.Tier, a.Mode, a.ResolvedCommand)

				// A full-tier adapter promises a parser that must exist in this
				// build, or the run would silently degrade to buffered output.
				if a.Tier == config.TierFull {
					if _, ok := stream.ParserFor(id); !ok {
						report(false, "adapter %s is tier full but this build has no parser for it", id)
					}
				}
				if !a.SandboxEnforcedOrDefault() {
					report(true, "adapter %s runs UNSANDBOXED (declared)", id)
				}
				if a.Worktree == config.WorktreeOff {
					report(true, "adapter %s writes UNCONFINED (worktree: off): no diff, no undo", id)
				}
				if a.Mode == config.ModeWrite {
					if _, err := exec.LookPath("git"); err != nil {
						report(false, "adapter %s writes but git is not installed; confined runs need it", id)
					}
				}
				// A CLI that no longer accepts its flags is the failure the
				// nightly conformance job exists to catch early.
				if err := probeCommand(a.ResolvedCommand); err != nil {
					report(false, "adapter %s: %v", id, err)
				}
			}

			var on []string
			for _, name := range []string{
				"stream", "watch", "interactive", "operator_steering",
				"agent_steering", "sessions", "audit", "progress_notifications",
			} {
				if cfg.Features.Enabled(name) {
					on = append(on, name)
				}
			}
			report(true, "features on: %v", on)
			if cfg.Features.Enabled("agent_steering") {
				report(true, "agent_steering is ON: agents may redirect each other without you")
			}
		}
	}

	// Live servers, if any, so the operator sees the same picture the TUI does.
	if entries := control.ReadIndex(paths.Runtime()); len(entries) > 0 {
		report(true, "%d live server(s) registered", len(entries))
	}

	if faults > 0 {
		return fmt.Errorf("%d fault(s)", faults)
	}
	fmt.Println("\nno faults found")
	return nil
}
