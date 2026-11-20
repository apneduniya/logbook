package hookrun

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/apneduniya/logbook/internal/agent/harness/aievent"
	"github.com/apneduniya/logbook/pkg/wire"
)

// StdinTimeout bounds how long the hook waits for the harness to finish
// writing its payload. Payloads normally arrive in a few milliseconds; this
// only matters for a harness that never closes stdin or a very slow machine,
// and then the prefix read so far is still used.
const StdinTimeout = time.Second

// Parser reads one harness's payloads.
type Parser interface {
	Parse(event string, in aievent.Input, getenv func(string) string) []aievent.Fields
	Response(event string) []byte
}

// Sender delivers events to the daemon or the spool.
type Sender interface {
	Send(ctx context.Context, events []wire.LocalEvent) error
}

// Runner runs hook invocations.
type Runner struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Now     func() time.Time
	Getenv  func(string) string
	Project aievent.ProjectFunc
	// Sender returns the sender to use; it is called only when there are
	// events, so a hook that records nothing doesn't load config.
	Sender func() (Sender, error)
	// PromptKey returns the key prompt hashes are made with
	// (localkey.PromptKey of the local token). It is called only for a
	// prompt_submit that carries a prompt digest. With no key (nil func,
	// error or empty key) the event is recorded without a PromptHash.
	PromptKey func() ([]byte, error)
	// Log reports problems (to the agent log, never stdout/stderr).
	Log func(msg string, args ...any)
}

// Run handles one invocation. p is nil for an unknown harness, which gets no
// response and records nothing.
func (r *Runner) Run(ctx context.Context, h wire.Harness, p Parser, event string) {
	defer func() {
		if v := recover(); v != nil {
			r.Log("hook panicked", "harness", string(h), "event", event, "panic", fmt.Sprint(v))
		}
	}()
	now := r.Now()
	rctx, cancel := context.WithTimeout(ctx, StdinTimeout)
	in := aievent.ReadInput(rctx, r.Stdin)
	cancel()
	if p == nil {
		return
	}
	if resp := p.Response(event); len(resp) > 0 {
		if _, err := r.Stdout.Write(resp); err != nil {
			r.Log("writing hook response", "err", err)
		}
	}
	events := r.events(h, p, event, in, now)
	if len(events) == 0 {
		return
	}
	s, err := r.Sender()
	if err != nil {
		r.Log("hook events dropped", "count", len(events), "err", err)
		return
	}
	if err := s.Send(ctx, events); err != nil {
		r.Log("hook events dropped", "count", len(events), "err", err)
	}
}

func (r *Runner) events(h wire.Harness, p Parser, event string, in aievent.Input, now time.Time) []wire.LocalEvent {
	fields := p.Parse(event, in, r.Getenv)
	out := make([]wire.LocalEvent, 0, len(fields))
	for _, f := range fields {
		ev := aievent.Build(h, f, now, r.Project)
		if ev.Kind == wire.AIPromptSubmit && f.PromptDigest.IsSet() {
			ev.PromptHash = aievent.PromptHash(f, r.promptKey())
		}
		if err := ev.Validate(); err != nil {
			r.Log("skipping invalid hook event", "harness", string(h), "event", event, "err", err)
			continue
		}
		out = append(out, wire.LocalEvent{Type: "ai", AI: &ev})
	}
	return out
}

// promptKey returns the prompt key, or nil (logged) when there is none.
func (r *Runner) promptKey() []byte {
	if r.PromptKey == nil {
		return nil
	}
	k, err := r.PromptKey()
	if err != nil {
		r.Log("prompt hash skipped", "err", err)
		return nil
	}
	return k
}
