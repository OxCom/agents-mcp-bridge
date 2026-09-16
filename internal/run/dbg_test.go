package run

import (
	"os"
	"testing"
	"time"

	"github.com/oxcom/agents-mcp-bridge/internal/platform"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

func TestDbgTranscript(t *testing.T) {
	reg := NewRegistry(4, time.Hour)
	sp := steerSpec(t, "hello")
	sp.Args = []string{"rawdrain", "not json at all"}
	sp.Timeout = 2 * time.Second
	r, _ := reg.Start(sp, platform.NewProcessGroup())
	s := r.Await(15 * time.Second)
	t.Logf("state=%q failure=%q diag=%q output=%q", s.State, s.Failure, s.Diagnostic, s.Output)
	raw, rerr := os.ReadFile(s.Transcript)
	t.Logf("file bytes=%d err=%v content=%q", len(raw), rerr, string(raw))
	e, ok := stream.ClaudeParser{}.Parse([]byte("not json at all"))
	t.Logf("direct parse: ok=%v kind=%s", ok, e.Kind)
	ev, err := stream.ReadTranscript(s.Transcript)
	t.Logf("read err=%v events=%d", err, len(ev))
	for _, e := range ev {
		t.Logf("  seq=%d kind=%s text=%q", e.Seq, e.Kind, e.Text)
	}
}
