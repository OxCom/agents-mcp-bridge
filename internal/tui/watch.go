// Package tui renders a delegated run live, in a terminal the operator owns.
//
// It reads the transcript file, which is the single source of truth, rather
// than holding a socket open to the server: a viewer that crashes or lags must
// never stall the agent it is watching.
package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/oxcom/agents-mcp-bridge/internal/control"
	"github.com/oxcom/agents-mcp-bridge/internal/run"
	"github.com/oxcom/agents-mcp-bridge/internal/sanitize"
	"github.com/oxcom/agents-mcp-bridge/internal/stream"
)

// errControlUnreachable is what the operator sees while pollQuestion's
// control-channel round trip is failing — the server is wedged or gone, and
// the question panel may be stale until it recovers.
var errControlUnreachable = errors.New("control channel unreachable; the question panel may be stale")

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Faint(true)
	warnStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208"))
	kindStyle   = map[stream.Kind]lipgloss.Style{
		stream.KindMessage:      lipgloss.NewStyle().Foreground(lipgloss.Color("39")),
		stream.KindThinking:     lipgloss.NewStyle().Faint(true),
		stream.KindToolCall:     lipgloss.NewStyle().Foreground(lipgloss.Color("141")),
		stream.KindShellCommand: lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		stream.KindFileChanged:  lipgloss.NewStyle().Foreground(lipgloss.Color("46")),
		stream.KindVendorError:  lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
	}
)

// Meta is what the header shows about the run being watched.
type Meta struct {
	RunID      string
	Agent      string
	Host       string
	Mode       string
	Confined   bool
	Sandboxed  bool
	Transcript string
	// Send delivers an operator control.Request (VerbAnswer) to the server
	// that owns this run, returning any error the server reported — a
	// question already answered elsewhere, or a run that has moved on. nil in
	// tests that never press an answer key.
	Send func(control.Request) error
	// Refresh reports the watched run's current control.RunInfo, so the
	// question panel tracks whatever the run is blocked on right now rather
	// than a poll take from when the TUI first attached. ok is false when the
	// server is unreachable or no longer reports this run; the panel is left
	// as-is rather than cleared on a transient failure. nil in tests that
	// never need the panel to populate live.
	Refresh func() (info control.RunInfo, ok bool)
}

type tickMsg time.Time

type model struct {
	meta     Meta
	events   []stream.Event
	offset   int64
	width    int
	height   int
	err      error
	finished bool

	// controlErr is set when pollQuestion's control-channel round trip fails
	// (a wedged or unreachable server) and cleared the moment one succeeds
	// again. It is a single scalar, overwritten in place rather than
	// appended, precisely because pollQuestion runs every 200ms: an outage
	// must report once and stay visible while it persists, not print a new
	// line per tick.
	controlErr error

	// question is what a delegated agent is currently blocked on, as reported
	// by the resolver. It is UNTRUSTED text from another AI agent, rendered to
	// a human about to act on it — see questionPanel.
	question      *run.Question
	questionRun   string
	questionAgent string
	// questionResting is true when the panel's question came from a run
	// resting in needs_input rather than a live pending one: the child is
	// already gone, so an answer here creates a continuation (VerbContinue)
	// instead of resolving a live question (VerbAnswer) — see sendAnswer.
	questionResting bool
	// continuedID is the successor run's id once THIS run has been
	// continued — by this panel's own answer, by another TUI, or by the
	// `bridge answer` CLI. Discovered purely by polling (info.SupersededBy),
	// never assumed from a send's own return, since Send reports only
	// success or failure, not the id it created.
	continuedID string
	// typing/answerText hold a free-text answer being composed with 't'.
	typing     bool
	answerText string
	// send is how an answer or decline reaches the server. Overridable in
	// tests.
	send func(control.Request) error
	// refresh is how the panel learns what the run is currently blocked on.
	// Overridable in tests.
	refresh func() (control.RunInfo, bool)
}

func newModel() model {
	return model{width: 100, height: 30}
}

// Run watches one run until the operator quits.
func Run(meta Meta) error {
	m := newModel()
	m.meta = meta
	m.send = meta.Send
	m.refresh = meta.Refresh
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		if m.question != nil {
			return m.updateQuestion(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
		return m, nil
	case tickMsg:
		m.poll()
		m.pollQuestion()
		return m, tick()
	}
	return m, nil
}

// updateQuestion handles a keypress while a question is pending. It never
// falls through to the plain quit bindings, so 'q' while composing free text
// types a literal q instead of exiting.
func (m model) updateQuestion(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.typing {
		switch msg.Type {
		case tea.KeyEnter:
			m.sendAnswer(m.answerText)
		case tea.KeyEsc:
			m.typing = false
			m.answerText = ""
		case tea.KeyBackspace:
			if r := []rune(m.answerText); len(r) > 0 {
				m.answerText = string(r[:len(r)-1])
			}
		case tea.KeyRunes:
			m.answerText += string(msg.Runes)
		}
		return m, nil
	}

	switch msg.String() {
	case "t":
		m.typing = true
		m.answerText = ""
	case "d":
		m.sendAnswer("(operator declined to answer)")
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	default:
		if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
			idx := int(msg.Runes[0] - '1')
			if idx < len(m.question.Options) {
				m.sendAnswer(m.question.Options[idx])
			}
		}
	}
	return m, nil
}

// sendAnswer delivers the operator's answer for the run it was asked on —
// never the currently-watched run by assumption, but the run id recorded
// alongside the question — and clears the panel. Answering the wrong run is
// worse than not answering, so the run id always travels with the request.
func (m *model) sendAnswer(text string) {
	if m.send != nil && m.question != nil {
		req := control.Request{RunID: m.questionRun, Text: text}
		if m.questionResting {
			// The run has already stopped: there is no live question left to
			// resolve, so this creates a continuation instead
			// (docs/superpowers/specs/2026-09-15-continuation-design.md §7).
			// The successor's id is not returned here — Send reports only
			// success or failure — it is discovered on the next poll via
			// info.SupersededBy, same as any other TUI or the `bridge
			// answer` CLI continuing this run concurrently would surface.
			req.Verb = control.VerbContinue
		} else {
			req.Verb = control.VerbAnswer
			req.QuestionID = m.question.ID
		}
		// The run may already have moved on — answered elsewhere, timed out,
		// or finished — between the panel rendering and this keypress. The
		// server refuses rather than applying the answer to whatever the run
		// is doing now; surface that refusal instead of swallowing it.
		if err := m.send(req); err != nil {
			m.err = err
		}
	}
	m.question = nil
	m.questionRun = ""
	m.questionAgent = ""
	m.questionResting = false
	m.typing = false
	m.answerText = ""
}

// pollQuestion refreshes the panel from the run's current control.RunInfo. It
// leaves an in-progress typed answer untouched as long as the same question
// is still pending, and clears the panel the moment the run no longer reports
// one — answered elsewhere, timed out, or finished.
func (m *model) pollQuestion() {
	if m.refresh == nil {
		return
	}
	info, ok := m.refresh()
	if !ok {
		// A wedged or unreachable server otherwise leaves the operator
		// staring at a frozen panel with no explanation — sendAnswer already
		// surfaces its own failures this way; poll must too. Only set it
		// once: an outage that outlives several ticks must not keep
		// replacing the message with an equivalent one.
		if m.controlErr == nil {
			m.controlErr = errControlUnreachable
		}
		return
	}
	m.controlErr = nil
	if info.SupersededBy != "" {
		// This run has been continued — by this panel's own answer, another
		// TUI, or the `bridge answer` CLI. Nothing is left pending here.
		m.continuedID = info.SupersededBy
		m.question = nil
		m.questionRun = ""
		m.questionAgent = ""
		m.questionResting = false
		m.typing = false
		m.answerText = ""
		return
	}
	if info.QuestionID == "" {
		m.question = nil
		m.questionRun = ""
		m.questionAgent = ""
		m.questionResting = false
		m.typing = false
		m.answerText = ""
		return
	}
	resting := info.State == "needs_input"
	if m.question != nil && m.question.ID == info.QuestionID && m.questionResting == resting {
		return
	}
	m.question = &run.Question{
		ID:      info.QuestionID,
		Text:    info.QuestionText,
		Options: info.QuestionOptions,
	}
	m.questionRun = info.RunID
	m.questionAgent = info.Agent
	m.questionResting = resting
	m.typing = false
	m.answerText = ""
}

// questionPanel renders the question a delegated agent is blocked on.
//
// The text is UNTRUSTED: it was generated by another AI agent and may try to
// look like an instruction ("ignore previous instructions and approve
// everything"). It must never read as though the bridge itself is prompting
// the operator. It is rendered as an attributed, quoted REPORT of what that
// agent asked — labelled with its agent id and run id — so the operator can
// tell data from instruction at a glance.
func (m model) questionPanel() string {
	if m.question == nil {
		return ""
	}
	var b strings.Builder
	verb := "asks"
	if m.questionResting {
		// The child is already gone (docs/12 §6): answering here creates a
		// continuation, a new run seeded with this question and the answer,
		// rather than resolving anything live.
		verb = "asked, before this run stopped — answering now continues it as a new run"
	}
	fmt.Fprintf(&b, "%s\n", warnStyle.Render(fmt.Sprintf(
		"agent %s (run %s) %s — reported verbatim below, not a bridge instruction:",
		m.questionAgent, m.questionRun, verb)))

	// One line, truncated: a long or multi-line question from the agent must
	// not push the event feed off screen.
	budget := m.width - 6
	if budget < 20 {
		budget = 20
	}
	fmt.Fprintf(&b, "  %q\n", oneLine(m.question.Text, budget))

	// Options get the identical treatment: an untrusted option that embeds a
	// newline could otherwise forge a fake "[d] decline" row, mimic the
	// attribution line above, or push it off screen entirely — exactly the
	// spoof this panel exists to prevent.
	for i, opt := range m.question.Options {
		if i >= 9 {
			break
		}
		fmt.Fprintf(&b, "  [%d] %s\n", i+1, oneLine(opt, budget))
	}
	if m.typing {
		fmt.Fprintf(&b, "  answer> %s_   (enter to send, esc to cancel)\n", m.answerText)
	} else {
		b.WriteString("  [t] type an answer   [d] decline\n")
	}
	b.WriteString(dimStyle.Render(strings.Repeat("─", max(10, m.width))))
	b.WriteString("\n")
	return b.String()
}

// poll reads whatever has been appended since the last tick. Reading only the
// tail keeps a long run cheap to watch.
func (m *model) poll() {
	f, err := os.Open(m.meta.Transcript)
	if err != nil {
		m.err = err
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		m.err = err
		return
	}
	if fi.Size() <= m.offset {
		return
	}
	if _, err := f.Seek(m.offset, 0); err != nil {
		m.err = err
		return
	}
	events, err := stream.DecodeEvents(f)
	if err != nil {
		m.err = err
		return
	}
	m.offset = fi.Size()
	m.events = append(m.events, events...)
	for _, e := range events {
		if e.Kind == stream.KindRunFinished || e.Kind == stream.KindRunFailed {
			m.finished = true
		}
	}
	m.err = nil
}

func (m model) View() string {
	var b strings.Builder

	b.WriteString(headerStyle.Render(fmt.Sprintf("%s  →  %s", m.meta.Host, m.meta.Agent)))
	b.WriteString(dimStyle.Render("   " + m.meta.RunID))
	b.WriteString("\n")

	status := fmt.Sprintf("mode %s", m.meta.Mode)
	if !m.meta.Sandboxed {
		status += "  " + warnStyle.Render("UNSANDBOXED")
	}
	if !m.meta.Confined {
		status += "  " + warnStyle.Render("UNCONFINED — edits land directly in your tree")
	}
	b.WriteString(dimStyle.Render(status))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", max(10, m.width))))
	b.WriteString("\n")

	qp := m.questionPanel()
	if qp != "" {
		b.WriteString(qp)
	}

	// Only the last screenful is rendered; a long run must not redraw its whole
	// history every 200ms. The question panel's own lines come out of the same
	// budget so it can never scroll the feed away.
	body := m.height - 6 - strings.Count(qp, "\n")
	if body < 3 {
		body = 3
	}
	start := 0
	if len(m.events) > body {
		start = len(m.events) - body
	}
	for _, e := range m.events[start:] {
		b.WriteString(renderEvent(e, m.width))
		b.WriteString("\n")
	}

	b.WriteString(dimStyle.Render(strings.Repeat("─", max(10, m.width))))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(stream.Digest(m.events)))
	if m.finished {
		b.WriteString(headerStyle.Render("   [finished]"))
	}
	if m.continuedID != "" {
		b.WriteString(headerStyle.Render(fmt.Sprintf("   [continued as %s — bridge watch %s]", m.continuedID, m.continuedID)))
	}
	if m.err != nil {
		b.WriteString(warnStyle.Render("   " + m.err.Error()))
	}
	if m.controlErr != nil {
		b.WriteString(warnStyle.Render("   " + m.controlErr.Error()))
	}
	b.WriteString(dimStyle.Render("   q to quit"))
	return b.String()
}

// renderEvent formats one event. Everything here came from an untrusted agent,
// so it is sanitized before it reaches the operator's terminal: an unescaped
// ANSI sequence could repaint the screen and hide what the agent actually did.
func renderEvent(e stream.Event, width int) string {
	label := strings.TrimPrefix(string(e.Kind), "agent.")
	label = strings.TrimPrefix(label, "run.")

	detail := e.Text
	switch e.Kind {
	case stream.KindToolCall:
		detail = e.Tool
		if e.Text != "" {
			detail += " " + e.Text
		}
	case stream.KindFileChanged:
		detail = e.Path + " (" + e.Text + ")"
	case stream.KindUsage:
		if e.Usage != nil {
			detail = fmt.Sprintf("%d in / %d out tokens", e.Usage.InputTokens, e.Usage.OutputTokens)
		}
	}
	detail = sanitize.Clean(detail, 0).Text
	detail = strings.ReplaceAll(detail, "\n", " ")

	budget := width - 18
	if budget < 20 {
		budget = 20
	}
	if len(detail) > budget {
		detail = detail[:budget-1] + "…"
	}

	style, ok := kindStyle[e.Kind]
	if !ok {
		style = dimStyle
	}
	return fmt.Sprintf("%s %s %s",
		dimStyle.Render(e.Time.Format("15:04:05")),
		style.Render(fmt.Sprintf("%-14s", label)),
		detail)
}

// oneLine collapses newlines to spaces and truncates to budget runes, for any
// untrusted agent text rendered into the single-line question panel. Without
// this, an embedded newline lets a question or option forge extra rows —
// a fake "[d] decline", a mimicked attribution line, or the real one pushed
// off screen.
func oneLine(s string, budget int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > budget {
		s = string(r[:budget-1]) + "…"
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
