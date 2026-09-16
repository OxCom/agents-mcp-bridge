package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/tui"
)

// runWatch renders one run live. With no run id it lists what is available,
// across every server this user is running.
func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := platform.NewPaths()
	if err != nil {
		return err
	}
	runs, err := collectRuns(paths.Runtime())
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return fmt.Errorf("no runs found; is a bridge serving, and has it delegated anything?")
	}

	want := fs.Arg(0)
	if want == "" {
		if len(runs) == 1 {
			want = runs[0].info.RunID
		} else {
			printRuns(runs)
			return fmt.Errorf("several runs are live; name one")
		}
	}

	for _, r := range runs {
		if r.info.RunID != want {
			continue
		}
		if r.info.Transcript == "" {
			return fmt.Errorf("run %s has no transcript: its adapter is basic tier, which produces no events", want)
		}

		c, err := control.Dial(r.socket)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", r.host, err)
		}
		runID := r.info.RunID
		// Attach marks the TUI as watching THIS run, so the gate's resolver
		// routes a question here instead of falling back to elicitation
		// (WatchersAttachedTo, internal/control/server.go). runWatch has
		// already resolved exactly one run by this point — there is no
		// in-TUI run picker that could later switch runs — so one attach
		// naming that run id for the life of the process is the whole
		// contract; an attach carrying no run id is a picker that claims no
		// run and would leave this run's questions unrouted (the C-NEW
		// regression this fixes). Detach on every exit path: the server also
		// decrements on connection close, so a crashed TUI cannot hold the
		// signal high, but a clean exit detaches explicitly rather than
		// waiting on that.
		if err := attachToRun(c, runID); err != nil {
			_ = c.Close()
			return fmt.Errorf("attaching to %s: %w", r.host, err)
		}
		defer func() {
			_, _ = c.Do(control.Request{Verb: control.VerbDetach})
			_ = c.Close()
		}()

		return tui.Run(tui.Meta{
			RunID:      r.info.RunID,
			Agent:      r.info.Agent,
			Host:       r.host,
			Mode:       r.info.Mode,
			Confined:   r.info.Confined,
			Sandboxed:  r.info.Sandboxed,
			Transcript: r.info.Transcript,
			Send: func(req control.Request) error {
				_, err := c.Do(req)
				return err
			},
			Refresh: func() (control.RunInfo, bool) {
				resp, err := c.Do(control.Request{Verb: control.VerbRuns})
				if err != nil {
					return control.RunInfo{}, false
				}
				for _, ri := range resp.Runs {
					if ri.RunID == runID {
						return ri, true
					}
				}
				// The run has aged out of the server's registry (past
				// retention) rather than merely lacking a question: report it
				// as "no question" so a stale panel still clears.
				return control.RunInfo{}, true
			},
		})
	}
	printRuns(runs)
	return fmt.Errorf("no run %q", want)
}

// runRuns prints the run table with ids, agents and ages across every live
// server (docs/04-config-schema.md:438).
func runRuns(args []string) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := platform.NewPaths()
	if err != nil {
		return err
	}
	runs, err := collectRuns(paths.Runtime())
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("no runs found; is a bridge serving, and has it delegated anything?")
		return nil
	}
	printRuns(runs)
	return nil
}

// attachToRun sends the production VerbAttach request, always carrying the
// run id being watched: an attach with no run id is a picker (I1) and never
// routes a question to this TUI. Pulled out of runWatch so a test can drive
// this exact call against a real control.Server rather than a hand-built
// request that no production path sends.
func attachToRun(c *control.Client, runID string) error {
	_, err := c.Do(control.Request{Verb: control.VerbAttach, RunID: runID})
	return err
}

type located struct {
	info   control.RunInfo
	host   string
	socket string
}

// collectRuns asks every live server for its runs. One host session per agent
// means several servers, and the operator should not have to know which is
// which.
func collectRuns(runtimeDir string) ([]located, error) {
	var out []located
	for _, entry := range control.ReadIndex(runtimeDir) {
		c, err := control.Dial(entry.Socket)
		if err != nil {
			continue // a server that died between index read and dial
		}
		resp, err := c.Do(control.Request{Verb: control.VerbRuns})
		_ = c.Close()
		if err != nil {
			continue
		}
		for _, r := range resp.Runs {
			out = append(out, located{info: r, host: entry.Host, socket: entry.Socket})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].info.Started.After(out[j].info.Started) })
	return out, nil
}

func printRuns(runs []located) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "RUN\tHOST\tAGENT\tSTATE\tMODE\tAGE")
	for _, r := range runs {
		age := time.Since(r.info.Started).Round(time.Second)
		warnings := ""
		if !r.info.Confined {
			warnings += " UNCONFINED"
		}
		if !r.info.Sandboxed {
			warnings += " UNSANDBOXED"
		}
		state := r.info.State
		if r.info.SupersededBy != "" {
			state += " -> " + r.info.SupersededBy
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s%s\t%s\n",
			r.info.RunID, r.host, r.info.Agent, state, r.info.Mode, warnings, age)
	}
	_ = w.Flush()
}

// runStatus prints what every live server is doing.
func runStatus(args []string) error {
	paths, err := platform.NewPaths()
	if err != nil {
		return err
	}
	entries := control.ReadIndex(paths.Runtime())
	if len(entries) == 0 {
		fmt.Println("no bridge servers are running")
		return nil
	}
	for _, e := range entries {
		c, err := control.Dial(e.Socket)
		if err != nil {
			fmt.Printf("pid %d (%s): unreachable\n", e.PID, e.Host)
			continue
		}
		resp, err := c.Do(control.Request{Verb: control.VerbStatus})
		_ = c.Close()
		if err != nil || resp.Status == nil {
			fmt.Printf("pid %d (%s): %v\n", e.PID, e.Host, err)
			continue
		}
		st := resp.Status
		fmt.Printf("pid %d  host %s  depth %d\n", st.PID, st.Host, st.Depth)
		fmt.Printf("  delegation targets: %v\n", st.Adapters)
		fmt.Printf("  features on: %v\n", st.Features)
	}

	runs, _ := collectRuns(paths.Runtime())
	if len(runs) > 0 {
		fmt.Println()
		printRuns(runs)
	}
	return nil
}

// runStop cancels a run from the operator side.
func runStop(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: bridge stop <run_id>")
	}
	paths, err := platform.NewPaths()
	if err != nil {
		return err
	}
	for _, e := range control.ReadIndex(paths.Runtime()) {
		c, err := control.Dial(e.Socket)
		if err != nil {
			continue
		}
		_, err = c.Do(control.Request{Verb: control.VerbStop, RunID: args[0]})
		_ = c.Close()
		if err == nil {
			fmt.Printf("stopped %s\n", args[0])
			return nil
		}
	}
	return fmt.Errorf("no live server owns run %s", args[0])
}
