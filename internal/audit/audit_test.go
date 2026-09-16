package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestWriter(t *testing.T, mutate func(*Options)) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	opts := Options{
		Path:    filepath.Join(dir, "audit.jsonl"),
		KeyFile: filepath.Join(dir, "key"),
	}
	if mutate != nil {
		mutate(&opts)
	}
	w, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, opts.Path
}

func readEntries(t *testing.T, path string) []Entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line is not valid JSON: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestBodiesAreAbsentByDefault(t *testing.T) {
	w, path := newTestWriter(t, nil)
	if err := w.Write(Entry{
		Event:    "run.completed",
		Prompt:   "the secret prompt",
		Response: "the secret response",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("bodies were written despite bodies=false:\n%s", raw)
	}
}

func TestBodiesWrittenOnlyWhenEnabled(t *testing.T) {
	w, path := newTestWriter(t, func(o *Options) { o.Bodies = true })
	if err := w.Write(Entry{Event: "run.completed", Prompt: "kept"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustRead(t, path)), "kept") {
		t.Fatal("bodies=true did not store the body")
	}
}

func TestDigestsAreKeyedNotBarehash(t *testing.T) {
	// An unsalted hash of a short predictable prompt is a dictionary oracle.
	dir1, dir2 := t.TempDir(), t.TempDir()
	w1, err := New(Options{Path: filepath.Join(dir1, "a.jsonl"), KeyFile: filepath.Join(dir1, "key")})
	if err != nil {
		t.Fatal(err)
	}
	defer w1.Close()
	w2, err := New(Options{Path: filepath.Join(dir2, "a.jsonl"), KeyFile: filepath.Join(dir2, "key")})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	const body = "yes"
	if w1.Digest(body) == w2.Digest(body) {
		t.Fatal("two installs produced the same digest; the key is not being used")
	}
	if first, second := w1.Digest(body), w1.Digest(body); first != second {
		t.Fatalf("digest is not stable within one install: %s vs %s", first, second)
	}
	if !strings.HasPrefix(w1.Digest(body), "hmac-sha256:") {
		t.Fatalf("digest is not labelled: %s", w1.Digest(body))
	}
}

func TestKeyPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Path: filepath.Join(dir, "a.jsonl"), KeyFile: filepath.Join(dir, "key")}
	w1, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	first := w1.Digest("x")
	_ = w1.Close()

	w2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if w2.Digest("x") != first {
		t.Fatal("digests must stay comparable across restarts")
	}
}

func TestKeyFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	w, err := New(Options{Path: filepath.Join(dir, "a.jsonl"), KeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestRefusalsAreRecorded(t *testing.T) {
	// A refusal that leaves no trace is indistinguishable from a call that was
	// never made.
	w, path := newTestWriter(t, nil)
	if err := w.Write(Entry{Event: "run.refused", Reason: "root_violation", TargetAgent: "codex"}); err != nil {
		t.Fatal(err)
	}
	entries := readEntries(t, path)
	if len(entries) != 1 || entries[0].Reason != "root_violation" {
		t.Fatalf("refusal not recorded: %#v", entries)
	}
}

func TestEveryLineIsIndependentlyParsable(t *testing.T) {
	w, path := newTestWriter(t, nil)
	for i := 0; i < 5; i++ {
		if err := w.Write(Entry{Event: "run.completed", RunID: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(readEntries(t, path)); got != 5 {
		t.Fatalf("read %d entries, want 5", got)
	}
}

func TestRotationPreservesHistoryAndPrunes(t *testing.T) {
	w, path := newTestWriter(t, func(o *Options) {
		o.RotateBytes = 200
		o.RetainFiles = 2
	})
	for i := 0; i < 40; i++ {
		if err := w.Write(Entry{Event: "run.completed", RunID: strings.Repeat("r", 20)}); err != nil {
			t.Fatal(err)
		}
	}
	rotated, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatal(err)
	}
	if len(rotated) == 0 {
		t.Fatal("log never rotated")
	}
	if len(rotated) > 2 {
		t.Fatalf("retain_files ignored: %d rotated files kept", len(rotated))
	}
}

func TestRedactArgvReplacesTheSensitiveElement(t *testing.T) {
	w, _ := newTestWriter(t, nil)
	argv := []string{"exec", "--json", "--", "the secret prompt"}
	got := w.RedactArgv(argv, "the secret prompt")
	if got[3] == "the secret prompt" {
		t.Fatal("the prompt was left in argv")
	}
	if !strings.HasPrefix(got[3], "hmac-sha256:") {
		t.Fatalf("prompt not replaced by a digest: %q", got[3])
	}
	for i := 0; i < 3; i++ {
		if got[i] != argv[i] {
			t.Fatalf("non-sensitive element %d altered: %q", i, got[i])
		}
	}
}

func TestNilWriterIsSafe(t *testing.T) {
	// Auditing can be disabled; every call site must stay simple.
	var w *Writer
	if err := w.Write(Entry{Event: "x"}); err != nil {
		t.Fatalf("nil writer must be a no-op: %v", err)
	}
	if w.Digest("x") != "" || w.Close() != nil {
		t.Fatal("nil writer must be inert")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
