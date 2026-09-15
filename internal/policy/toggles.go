package policy

import (
	"fmt"
	"sync"
	"time"
)

// Toggles are the operator's live switches.
//
// A toggle may only NARROW what the config file permits. There is no runtime
// path that widens authority: the config is the ceiling, and a toggle turning
// something back on can only restore what the ceiling already allows. No MCP
// tool can reach this — the model cannot change its own limits.
type Toggles struct {
	mu  sync.RWMutex
	off map[string]bool
}

// NewToggles returns an empty toggle set: everything the config allows is on.
func NewToggles() *Toggles { return &Toggles{off: make(map[string]bool)} }

// Disable turns a feature off for this server instance.
func (t *Toggles) Disable(feature string) { t.set(feature, true) }

// Enable clears a runtime override. It cannot exceed the config ceiling,
// because the ceiling is consulted separately and independently.
func (t *Toggles) Enable(feature string) { t.set(feature, false) }

func (t *Toggles) set(feature string, off bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.off[feature] = off
}

// DisabledAtRuntime reports whether the operator switched a feature off.
func (t *Toggles) DisabledAtRuntime(feature string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.off[feature]
}

// Snapshot lists the features the operator has switched off.
func (t *Toggles) Snapshot() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []string
	for f, off := range t.off {
		if off {
			out = append(out, f)
		}
	}
	return out
}

// SetToggles attaches an operator toggle set to the engine.
func (e *Engine) SetToggles(t *Toggles) { e.toggles = t }

// KnownFeature reports whether a name is a feature at all, so the operator gets
// a typo rejected rather than silently accepted.
func KnownFeature(name string) bool {
	switch name {
	case "stream", "watch", "interactive", "operator_steering",
		"agent_steering", "sessions", "audit", "progress_notifications":
		return true
	default:
		return false
	}
}

// rateLimiter bounds how often a caller may start runs, independent of how many
// run at once: a caller that starts and cancels in a loop would never trip a
// concurrency ceiling.
type rateLimiter struct {
	mu      sync.Mutex
	times   []time.Time
	perMin  int
	nowFunc func() time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{perMin: perMinute, nowFunc: time.Now}
}

// allow records an attempt and reports whether it is within the limit.
func (r *rateLimiter) allow() bool {
	if r == nil || r.perMin <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.nowFunc()
	cutoff := now.Add(-time.Minute)
	kept := r.times[:0]
	for _, t := range r.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.times = kept
	if len(r.times) >= r.perMin {
		return false
	}
	r.times = append(r.times, now)
	return true
}

// ReasonRateLimited is returned when a caller starts runs too quickly.
const ReasonRateLimited Reason = "rate_limited"

// checkRate is called by Authorise. It is separate so the limiter can be tested
// without spawning anything.
func (e *Engine) checkRate() error {
	if e.rate != nil && !e.rate.allow() {
		return refuse(ReasonRateLimited, "too many runs started recently")
	}
	return nil
}

var _ = fmt.Sprintf
