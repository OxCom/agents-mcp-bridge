package stream

import (
	"strings"
	"testing"
)

// digestEvents is one of each kind a digest counts, with vendor-derived
// strings in the two fields a caller-facing digest must not repeat.
func digestEvents() []Event {
	return []Event{
		{Kind: KindToolCall, Tool: "custom_vendor_tool"},
		{Kind: KindFileChanged, Path: "/tmp/vendor-secret-path.go"},
		{Kind: KindShellCommand},
		{Kind: KindMessage},
		{Kind: KindUsage, Usage: &Usage{InputTokens: 11, OutputTokens: 22}},
	}
}

// TestDigestCountsCarriesNoVendorDerivedStrings pins the caller-facing digest.
// It leaves the envelope as `notice` and as a progress message, so every
// vendor-derived string in it is vendor words outside the envelope.
func TestDigestCountsCarriesNoVendorDerivedStrings(t *testing.T) {
	d := DigestCounts(digestEvents())
	for _, forbidden := range []string{"custom_vendor_tool", "/tmp/vendor-secret-path.go"} {
		if strings.Contains(d, forbidden) {
			t.Fatalf("the counts-only digest repeated vendor-derived text %q: %s", forbidden, d)
		}
	}
	for _, want := range []string{"1 tool calls", "1 shell commands", "1 file changes", "1 messages"} {
		if !strings.Contains(d, want) {
			t.Fatalf("the counts-only digest lost %q: %s", want, d)
		}
	}
	if !strings.Contains(d, "11 in / 22 out tokens") {
		t.Fatalf("token counts are numbers, not vendor words, and must survive: %s", d)
	}
}

// TestDigestKeepsTheLastToolAndFileForTheOperator keeps the TUI's digest
// unnarrowed: the operator is reading the raw stream in the same window.
func TestDigestKeepsTheLastToolAndFileForTheOperator(t *testing.T) {
	d := Digest(digestEvents())
	for _, want := range []string{"custom_vendor_tool", "/tmp/vendor-secret-path.go"} {
		if !strings.Contains(d, want) {
			t.Fatalf("the operator's digest lost %q: %s", want, d)
		}
	}
}

func TestDigestCountsOnAnEmptyStream(t *testing.T) {
	if got := DigestCounts(nil); got != "no activity yet" {
		t.Fatalf("DigestCounts(nil) = %q", got)
	}
}
