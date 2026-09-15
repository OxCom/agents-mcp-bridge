package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/oxcom/agents-mcp-bridge/internal/adapter"
	"github.com/oxcom/agents-mcp-bridge/internal/audit"
	"github.com/oxcom/agents-mcp-bridge/internal/config"
	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/gate"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/policy"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	host := fs.String("host", "", "agent_id of the agent hosting this server (required)")
	configPath := fs.String("config", "", "config file path (default: the per-user location)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Logs go to stderr: stdout is the MCP transport and must carry nothing else.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	warning, err := config.VerifyHost(*host, config.DetectHost())
	if err != nil {
		return err
	}
	if warning != "" {
		log.Warn(warning)
	}

	paths, err := platform.NewPaths()
	if err != nil {
		return err
	}
	if *configPath == "" {
		*configPath = paths.Config()
	}

	cfg, err := config.Load(*configPath, config.Options{Host: *host})
	if err != nil {
		return fmt.Errorf("config %s: %w", *configPath, err)
	}
	// Semantic rule 2: a tier-full adapter promises streaming, sessions and
	// steering, all of which need a parser compiled into this build. The
	// loader cannot check it — it must not depend on the stream package — so
	// the check lives here, and it is fatal rather than a degradation.
	for id, a := range cfg.Agents {
		if a.Tier != config.TierFull {
			continue
		}
		if _, ok := stream.ParserFor(id); !ok {
			return fmt.Errorf("adapter %q is declared tier: full but this build has no parser for it; "+
				"use tier: basic for one-shot delegation, or build an adapter for %s", id, id)
		}
	}

	log.Info("configuration loaded",
		"host", *host,
		"adapters", len(cfg.Agents),
		"depth", adapter.DepthFromEnv(),
		"state", paths.State(),
		"runtime", paths.Runtime())

	depth := adapter.DepthFromEnv()
	engine, err := policy.New(cfg, platform.NewPathGuard(), *host, depth)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}

	var auditor *audit.Writer
	if cfg.Features.Enabled("audit") {
		auditPath := cfg.Audit.Path
		if auditPath == "" {
			auditPath = filepath.Join(paths.State(), "audit.jsonl")
		}
		keyFile := cfg.Audit.HMACKeyFile
		if keyFile == "" {
			keyFile = filepath.Join(paths.State(), "audit.key")
		}
		auditor, err = audit.New(audit.Options{
			Path:        auditPath,
			Bodies:      cfg.Audit.Bodies,
			RotateBytes: cfg.Audit.RotateBytes,
			RetainFiles: cfg.Audit.RetainFiles,
			KeyFile:     keyFile,
		})
		if err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		defer auditor.Close()
	}

	// A crashed process leaves its gate-*.json files behind: recordCompletion
	// and the shutdown defer both skip on a crash, so nothing else ever
	// removes them. Sweep before any run of this process can start its own
	// gate, so a stale file's run ID can never collide with a live one.
	sweepStaleGateConfigs(paths.State(), paths.Runtime(), log)

	runs := run.NewRegistry(cfg.Defaults.MaxConcurrentRuns,
		time.Duration(cfg.Defaults.RunRetentionS)*time.Second)
	// The needs_input -> failed expiry (internal/run.Run.expireIfPastDeadlineLocked)
	// has no other audit sink in reach: it can fire lazily inside a read path
	// (Snapshot) with nothing else recording it. Wire it to the same audit
	// log every other transition uses, closing the docs/11 §3 invariant 5 gap.
	// auditor.Write is nil-safe, so this is a no-op when the audit feature is
	// off.
	runs.OnTransition(func(runID, agent, event, detail string) {
		_ = auditor.Write(audit.Entry{Event: event, RunID: runID, TargetAgent: agent, Message: detail})
	})

	b := &bridge{
		cfg: cfg, engine: engine, runs: runs, audit: auditor,
		host: *host, depth: depth,
		transcriptDir: filepath.Join(paths.State(), "transcripts"),
		stateDir:      paths.State(),
		changes:       newChangeStore(),
		runtimeDir:    paths.Runtime(),
		log:           log,
		gates:         make(map[string]*gate.Server),
		awaiting:      make(map[string]*mcp.CallToolRequest),
	}
	// A worktree that outlives its server is a leak, not a feature.
	defer b.changes.discardAll()
	// A gate socket or mcp-config that outlives its server is a leak, not a feature.
	defer b.closeAllGates()
	// Defers run LIFO: declared last, so it runs FIRST, before closeAllGates
	// above. A run being cancelled at shutdown may still be mid-question; its
	// gate must still be listening to hand back a clean deny when the child
	// asks, rather than the child hitting a socket that is already gone. Do
	// not move this above the closeAllGates defer — that would tear the
	// gate out from under a still-live child instead of letting it fail
	// closed on its own.
	defer runs.CancelAll()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "agents-mcp-bridge",
		Version: version,
	}, nil)
	registerTools(server, b)
	if engine.Exhausted() {
		log.Warn("delegation depth exhausted; no delegation tools are exposed",
			"depth", depth, "max_depth", cfg.Defaults.MaxDepth)
	}

	ctrl, err := control.Listen(paths.Runtime(), platform.NewControlEndpoint(), b, log)
	if err != nil {
		return fmt.Errorf("control channel: %w", err)
	}
	defer ctrl.Close()
	b.control = ctrl
	log.Info("control channel ready", "socket", ctrl.Socket())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// stdio only. There is never a network listener.
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
