package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// stubAgentVerb is the argv element that selects the stub. `bridge validate`
// prepends it to the adapter's own command so a third party's adapter can be
// exercised against this binary instead of a vendor CLI.
const stubAgentVerb = "stub-agent"

// stubMarker is the first key of the echo record. The validator looks for this
// exact string in the enveloped run output, which is the only way it can tell
// the stub's report apart from anything else the adapter's argv might produce.
const stubMarker = `"stub_agent":1`

// stubEcho is what the stub prints in echo mode: the argv it was handed after
// its own leading flags, and everything that arrived on stdin. Both are what
// `bridge validate` asserts on to prove prompt delivery.
//
// It is emitted as ONE line of JSON so it survives internal/sanitize intact:
// the sanitizer strips control characters, and json.Marshal has already
// escaped every one of them into printable \u form.
type stubEcho struct {
	StubAgent  int      `json:"stub_agent"`
	Argv       []string `json:"argv"`
	Stdin      string   `json:"stdin"`
	StdinBytes int      `json:"stdin_bytes"`
}

// runStubAgent impersonates a vendor CLI so an adapter declaration can be
// exercised with no credentials, no network and no vendor binary installed.
//
// It is INERT BY CONSTRUCTION, and must stay that way: it reads no
// configuration, opens no socket, connects to nothing, creates no file, spawns
// no process, and ignores every environment variable. The only input it acts
// on is its own argv — and, in --emit mode, the one file named there. This
// matters because the stub ships inside the release binary: a stub that grew a
// config path, a socket or a write path would be an attack surface reachable
// from any argv an adapter author controls.
//
// Leading flags it consumes, in any order, before the first argument it does
// not recognise:
//
//	--emit <file>      write that file to stdout line by line instead of the
//	                   echo record, so a tier: full adapter's own stream.Parser
//	                   can be driven from a recorded vendor stream
//	--sleep <dur>      hold before exiting, so a timeout can be exercised
//	--exit <code>      exit non-zero, so failure handling can be exercised
//
// Everything after those is the adapter author's own argv and is echoed back
// untouched. Scanning stops at the first unrecognised element on purpose: an
// adapter whose own flags happen to be spelled --emit or --exit cannot make
// the stub act on them.
func runStubAgent(args []string) error {
	var (
		emit  string
		sleep time.Duration
		code  int
	)

	i := 0
	for i < len(args) {
		name, value, joined := splitStubFlag(args[i])
		if name != "--emit" && name != "--sleep" && name != "--exit" {
			break
		}
		if !joined {
			if i+1 >= len(args) {
				return fmt.Errorf("stub-agent: %s needs a value", name)
			}
			value = args[i+1]
			i++
		}
		switch name {
		case "--emit":
			emit = value
		case "--sleep":
			d, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("stub-agent: --sleep %q: %w", value, err)
			}
			sleep = d
		case "--exit":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("stub-agent: --exit %q: %w", value, err)
			}
			code = n
		}
		i++
	}
	rest := args[i:]

	if emit != "" {
		if err := emitFile(emit, os.Stdout); err != nil {
			return err
		}
	} else {
		if err := writeStubEcho(rest, os.Stdin, os.Stdout); err != nil {
			return err
		}
	}

	if sleep > 0 {
		time.Sleep(sleep)
	}
	if code != 0 {
		// Reported through the same channel a real CLI uses, so the run
		// manager's failure path is what the validator observes.
		fmt.Fprintf(os.Stderr, "stub-agent: exiting with status %d as instructed\n", code)
		os.Exit(code)
	}
	return nil
}

// splitStubFlag accepts both "--flag value" and "--flag=value". joined reports
// the second form, where the value is already in hand.
func splitStubFlag(arg string) (name, value string, joined bool) {
	if eq := strings.Index(arg, "="); eq > 0 {
		return arg[:eq], arg[eq+1:], true
	}
	return arg, "", false
}

// writeStubEcho reports the argv and the stdin the stub was given. Reading
// stdin to EOF is what makes an adapter declaring prompt: stdin observable;
// for prompt: argv there is nothing on stdin and the read returns empty.
func writeStubEcho(argv []string, in io.Reader, out io.Writer) error {
	body, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return fmt.Errorf("stub-agent: read stdin: %w", err)
	}
	if argv == nil {
		argv = []string{}
	}
	raw, err := json.Marshal(stubEcho{
		StubAgent:  1,
		Argv:       argv,
		Stdin:      string(body),
		StdinBytes: len(body),
	})
	if err != nil {
		return fmt.Errorf("stub-agent: encode: %w", err)
	}
	_, err = fmt.Fprintf(out, "%s\n", raw)
	return err
}

// emitFile replays a recorded vendor stream. It is a plain copy, line by line:
// the point is that the adapter's own parser, not the stub, decides what the
// lines mean.
func emitFile(path string, out io.Writer) error {
	// #nosec G304 -- path comes from the operator's own --stream-fixture flag,
	// passed through `bridge validate`. The stub reads it and nothing else.
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("stub-agent: --emit: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if _, err := fmt.Fprintf(out, "%s\n", sc.Bytes()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("stub-agent: --emit %s: %w", path, err)
	}
	return nil
}
