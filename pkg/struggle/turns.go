package struggle

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/apneduniya/logbook/pkg/wire"
)

// span is a closed time range [from, to].
type span struct {
	from, to time.Time
}

// turn is one agent turn: from a prompt_submit (or a tool_start outside any
// turn) until turn_end, session_end, the session's next prompt, or the last
// event before a turnTimeout gap.
type turn struct {
	session    string
	start, end time.Time
	prompt     bool // started by prompt_submit
	chars      int
	hash       string
	// endedByTurnEnd: the agent said it finished (turn_end), as opposed to a
	// timeout, an interruption by the next prompt or the end of the session.
	endedByTurnEnd bool
	// waits are the spans spent waiting on a permission ask; the agent isn't
	// busy then, the person is needed.
	waits []span
	// project is the project of the turn's first event that has one. Taking
	// the first (not the most frequent) keeps the agent's view of a running
	// turn the same as a later re-run's.
	project string
}

// busySpans returns the turn's span minus its permission waits.
func (t turn) busySpans() []span {
	out := make([]span, 0, len(t.waits)+1)
	from := t.start
	for _, w := range t.waits {
		if w.from.After(from) {
			out = append(out, span{from, w.from})
		}
		from = w.to
	}
	if !from.After(t.end) {
		out = append(out, span{from, t.end})
	}
	return out
}

// sessionState tracks one AI session while events are replayed.
type sessionState struct {
	open      *turn
	last      time.Time
	waitFrom  time.Time
	waiting   bool
	completed []turn
}

func (s *sessionState) close(at time.Time, byTurnEnd bool) {
	if s.open == nil {
		return
	}
	if s.waiting {
		s.open.waits = append(s.open.waits, span{s.waitFrom, at})
		s.waiting = false
	}
	s.open.end = at
	s.open.endedByTurnEnd = byTurnEnd
	s.completed = append(s.completed, *s.open)
	s.open = nil
}

// buildTurns replays AI events session by session and returns every turn,
// sorted by (start, session). horizon is how far the data reaches: a turn
// still open with its last event less than turnTimeout before horizon runs
// to horizon; otherwise it timed out at its last event.
func buildTurns(events []aiEvent, horizon time.Time) []turn {
	states := map[string]*sessionState{}
	var order []string
	for _, e := range events {
		st := states[e.session]
		if st == nil {
			st = &sessionState{}
			states[e.session] = st
			order = append(order, e.session)
		}
		if st.open != nil && e.t.Sub(st.last) > turnTimeout {
			st.close(st.last, false)
		}
		if st.waiting && e.kind != wire.AIPermissionAsk {
			st.open.waits = append(st.open.waits, span{st.waitFrom, e.t})
			st.waiting = false
		}
		switch e.kind {
		case wire.AIPromptSubmit:
			st.close(e.t, false)
			st.open = &turn{session: e.session, start: e.t, prompt: true, chars: e.chars, hash: e.hash}
		case wire.AIToolStart:
			if st.open == nil {
				st.open = &turn{session: e.session, start: e.t}
			}
		case wire.AIPermissionAsk:
			if st.open != nil && !st.waiting {
				st.waitFrom, st.waiting = e.t, true
			}
		}
		if st.open != nil && st.open.project == "" {
			st.open.project = e.project
		}
		switch e.kind {
		case wire.AITurnEnd:
			st.close(e.t, true)
		case wire.AISessionEnd:
			st.close(e.t, false)
		}
		st.last = e.t
	}
	var turns []turn
	slices.Sort(order)
	for _, id := range order {
		st := states[id]
		if st.open != nil {
			if horizon.Sub(st.last) > turnTimeout {
				st.close(st.last, false)
			} else {
				st.close(horizon, false)
			}
		}
		turns = append(turns, st.completed...)
	}
	slices.SortStableFunc(turns, func(a, b turn) int {
		return cmp.Or(a.start.Compare(b.start), strings.Compare(a.session, b.session))
	})
	return turns
}
