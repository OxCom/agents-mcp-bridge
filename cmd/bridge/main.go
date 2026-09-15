// Command bridge is the agents-mcp-bridge binary. One executable, three roles
// selected by the first argument: an MCP server spawned by a host agent, an
// operator TUI, and operator control verbs.
package main

import (
	"fmt"
	"os"
)

const usage = `bridge — delegate work from one CLI coding agent to another

  bridge serve --host <agent_id>   MCP server over stdio; spawned by the host agent
  bridge gate                      permission/question sink; spawned by a bridge run, not by hand
  bridge watch [run_id]            operator TUI; live view of a delegated run
  bridge runs                      run table with ids, agents, ages
  bridge status                    effective configuration, live runs, toggles
  bridge stop <run_id>             cancel a run
  bridge accept <run_id>           apply a confined write run's changes
  bridge reject <run_id>           discard them
  bridge steer <run_id> "<text>"   send guidance into a running agent
  bridge answer <run_id> "<text>"  answer a question the agent is waiting on
  bridge enable|disable <feature>  narrow or restore a feature on live servers
  bridge doctor                    validate config, permissions, adapters, host detection
  bridge version                   print the version

The same binary is registered in every agent's MCP config; only --host differs.
`

var version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "gate":
		err = runGate(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	case "watch":
		err = runWatch(os.Args[2:])
	case "runs":
		err = runRuns(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "stop":
		err = runStop(os.Args[2:])
	case "accept":
		err = runAccept(os.Args[2:])
	case "reject":
		err = runReject(os.Args[2:])
	case "steer":
		err = runSteerCmd(os.Args[2:])
	case "answer":
		err = runAnswerCmd(os.Args[2:])
	case "enable":
		err = runFeature("enable", os.Args[2:])
	case "disable":
		err = runFeature("disable", os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bridge: %v\n", err)
		os.Exit(1)
	}
}
