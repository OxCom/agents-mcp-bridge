package run

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// helperEnv marks a re-exec of this test binary as a stand-in child process.
// Windows has no sh, echo, sleep, cat, pwd or /bin/sh, so every test that needs
// a child process spawns this binary instead and names a behaviour as argv[1].
const helperEnv = "BRIDGE_RUN_TEST_HELPER"

// TestMain runs the requested behaviour and exits when helperEnv is set,
// instead of running the suite.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "" {
		os.Exit(m.Run())
	}
	os.Exit(runHelper(os.Args[1:]))
}

// helperCommand is the path tests put in Spec.Command to reach runHelper.
func helperCommand(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	return exe
}

// helperEnviron is Spec.Env for a child that must land in runHelper.
func helperEnviron() []string {
	return append(os.Environ(), helperEnv+"=1")
}

// runHelper performs one behaviour and returns the child's exit code.
//
//	echo <word>...          print the words, space separated, to stdout
//	fail <out> <err> <code> print out, print err on stderr, exit code
//	sleep <duration>        sleep for a Go duration string
//	bytes <n>               write n bytes of 'x' to stdout
//	cat                     copy stdin to stdout until EOF
//	pwd                     print the working directory
//	emit <line>...          print each line verbatim, then exit
//	rawdrain <line>         print the line, then read stdin to EOF, discarding
//	fakeagent               the stream-json echo agent described below
func runHelper(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "helper: no behaviour named")
		return 2
	}
	switch args[0] {
	case "echo":
		fmt.Println(strings.Join(args[1:], " "))
	case "fail":
		if len(args) != 4 {
			return helperUsage("fail <stdout> <stderr> <code>")
		}
		fmt.Println(args[1])
		fmt.Fprintln(os.Stderr, args[2])
		code, err := strconv.Atoi(args[3])
		if err != nil {
			return helperUsage("fail <stdout> <stderr> <code>")
		}
		return code
	case "sleep":
		if len(args) != 2 {
			return helperUsage("sleep <duration>")
		}
		d, err := time.ParseDuration(args[1])
		if err != nil {
			return helperUsage("sleep <duration>")
		}
		time.Sleep(d)
	case "bytes":
		if len(args) != 2 {
			return helperUsage("bytes <n>")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return helperUsage("bytes <n>")
		}
		writeFiller(n)
	case "cat":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 1
		}
	case "pwd":
		wd, err := os.Getwd()
		if err != nil {
			return 1
		}
		fmt.Println(wd)
	case "emit":
		for _, line := range args[1:] {
			fmt.Println(line)
		}
	case "rawdrain":
		if len(args) != 2 {
			return helperUsage("rawdrain <line>")
		}
		fmt.Println(args[1])
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "fakeagent":
		fakeAgent()
	default:
		fmt.Fprintf(os.Stderr, "helper: unknown behaviour %q\n", args[0])
		return 2
	}
	return 0
}

func helperUsage(form string) int {
	fmt.Fprintf(os.Stderr, "helper: usage: %s\n", form)
	return 2
}

// writeFiller emits n bytes in chunks so the parent has to drain the pipe
// rather than the whole payload fitting in the kernel buffer.
func writeFiller(n int) {
	chunk := strings.Repeat("x", 4096)
	for n > 0 {
		size := len(chunk)
		if n < size {
			size = n
		}
		if _, err := io.WriteString(os.Stdout, chunk[:size]); err != nil {
			return
		}
		n -= size
	}
}

// contentPattern mirrors the sed expression the shell stand-in used:
// the LAST "content":"..." on the line, or no match at all.
var contentPattern = regexp.MustCompile(`^.*"content":"([^"]*)"`)

// fakeAgent speaks enough stream-json to exercise steering without spending
// vendor credits: it echoes each user message it receives as an assistant
// message, and emits a result when told to finish. The echo is a template
// substitution, not a JSON encoder, so a steer message that escaped its own
// encoding on the way in would surface as an injected record on the way out.
func fakeAgent() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), MaxSteerBytes+(4<<10))
	for sc.Scan() {
		content := ""
		if m := contentPattern.FindStringSubmatch(sc.Text()); m != nil {
			content = m[1]
		}
		if content == "finish" {
			fmt.Println(`{"type":"result","subtype":"success","session_id":"sess-1","result":"done"}`)
			continue
		}
		fmt.Fprintf(os.Stdout,
			`{"type":"assistant","session_id":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"heard %s"}]}}`+"\n",
			content)
	}
}
