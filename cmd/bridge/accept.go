package main

import (
	"fmt"
	"strings"

	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
)

// runAccept applies a confined write run's diff from the operator's side.
//
// This is the default route: agent-side acceptance is off unless the operator
// enabled it, so by default a human decides what reaches the working tree.
func runAccept(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bridge accept <run_id>")
	}
	return controlVerb(control.VerbAccept, args[0], "applied")
}

// runReject discards a confined write run's changes and removes its worktree.
func runReject(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bridge reject <run_id>")
	}
	return controlVerb(control.VerbReject, args[0], "discarded")
}

// runSteerCmd injects operator guidance into a live run.
func runSteerCmd(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(`usage: bridge steer <run_id> "<message>"`)
	}
	return controlVerbText(control.VerbSteer, args[0], strings.Join(args[1:], " "), "steered")
}

// runAnswerCmd answers a question the agent is waiting on, or — if the run
// has already stopped in needs_input — creates a continuation instead
// (docs/superpowers/specs/2026-09-15-continuation-design.md §7). Operator
// only: this command's control-channel verbs (VerbAnswer, VerbContinue) are
// never reachable from the MCP surface a delegated agent holds.
//
// It resolves the run's currently pending question id itself, rather than
// sending an empty one, before answering. An empty QuestionID means "answer
// whatever is pending" (run.Run.AnswerQuestion) — the right default for an
// internal caller with nothing rendered to bind to, but wrong here: it would
// let a stale `bridge answer` invocation, typed against a question the
// operator already saw resolved, land on a different question that replaced
// it. The TUI is protected because it always sends the id it rendered; this
// makes the CLI do the same by asking the server what is pending right
// before it answers. There is no escape hatch to answer blind — a run with
// nothing pending has nothing this command should silently do.
func runAnswerCmd(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf(`usage: bridge answer <run_id> "<text>"`)
	}
	return answerRun(args[0], strings.Join(args[1:], " "))
}

// answerRun finds the live server that owns runID, and either answers a live
// question or creates a continuation, depending on what the run reports right
// now. The lookup and the answer are two round trips, so this can in
// principle change between them — exactly the same window
// run.Run.AnswerQuestion's id check exists to close: a stale id is refused
// with ErrQuestionChanged rather than misdelivered, and a stale needs_input
// read is refused by TakeContinuation's own state check rather than seeding
// a continuation from a run that has since moved on.
func answerRun(runID, text string) error {
	paths, err := platform.NewPaths()
	if err != nil {
		return err
	}
	entries := control.ReadIndex(paths.Runtime())
	if len(entries) == 0 {
		return fmt.Errorf("no bridge servers are running")
	}
	var lastErr error
	for _, e := range entries {
		c, err := control.Dial(e.Socket)
		if err != nil {
			continue
		}
		resp, err := c.Do(control.Request{Verb: control.VerbRuns})
		if err != nil {
			_ = c.Close()
			lastErr = err
			continue
		}
		var info control.RunInfo
		owns := false
		for _, ri := range resp.Runs {
			if ri.RunID == runID {
				owns, info = true, ri
				break
			}
		}
		if !owns {
			_ = c.Close()
			continue
		}
		// State, not QuestionID, is what distinguishes these: a needs_input
		// run still reports its last question's id (run.Snapshot retains it
		// for exactly this read), so checking QuestionID first would route a
		// stopped run's answer down the live-answer path, which has nothing
		// left listening for it.
		if info.State == "needs_input" {
			resp, err := c.Do(control.Request{Verb: control.VerbContinue, RunID: runID, Text: text})
			_ = c.Close()
			if err != nil {
				return err
			}
			fmt.Printf("run %s continued as %s\n", runID, resp.SuccessorID)
			return nil
		}
		if info.QuestionID != "" {
			_, err = c.Do(control.Request{Verb: control.VerbAnswer, RunID: runID, QuestionID: info.QuestionID, Text: text})
			_ = c.Close()
			if err == nil {
				fmt.Printf("answered %s\n", runID)
				return nil
			}
			return err
		}
		_ = c.Close()
		return fmt.Errorf("run %s has no question pending to answer", runID)
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no live server owns run %s", runID)
}

// runFeature switches a feature on or off on every live server.
func runFeature(verb string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bridge %s <feature>", verb)
	}
	paths, err := platform.NewPaths()
	if err != nil {
		return err
	}
	entries := control.ReadIndex(paths.Runtime())
	if len(entries) == 0 {
		return fmt.Errorf("no bridge servers are running")
	}
	var lastErr error
	applied := 0
	for _, e := range entries {
		c, err := control.Dial(e.Socket)
		if err != nil {
			continue
		}
		_, err = c.Do(control.Request{Verb: verb, Feature: args[0]})
		_ = c.Close()
		if err != nil {
			lastErr = err
			continue
		}
		applied++
	}
	if applied == 0 {
		if lastErr != nil {
			return lastErr
		}
		return fmt.Errorf("no server accepted the change")
	}
	fmt.Printf("%sd %s on %d server(s)\n", verb, args[0], applied)
	return nil
}

func controlVerb(verb, runID, past string) error {
	return controlVerbText(verb, runID, "", past)
}

func controlVerbText(verb, runID, text, past string) error {
	paths, err := platform.NewPaths()
	if err != nil {
		return err
	}
	entries := control.ReadIndex(paths.Runtime())
	if len(entries) == 0 {
		return fmt.Errorf("no bridge servers are running")
	}
	var lastErr error
	for _, e := range entries {
		c, err := control.Dial(e.Socket)
		if err != nil {
			continue
		}
		_, err = c.Do(control.Request{Verb: verb, RunID: runID, Text: text})
		_ = c.Close()
		if err == nil {
			fmt.Printf("%s %s\n", past, runID)
			return nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no live server owns run %s", runID)
}
