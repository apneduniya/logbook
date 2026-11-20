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

// turn is one agent turn: from a prompt_submit (or a tool_start or an
// unanswered permission_ask outside any turn) until turn_end, session_end, the session's next prompt, or the last
// event before a turnTimeout gap.
type turn struct {
	session    string
	start, end time.Time
	prompt     bool // started by prompt_submit
	chars      int
	hash       string
	// promptProject is the prompt_submit's project ("" without a prompt).
	promptProject string
	// afterTurnEnd: the session's event just before the prompt was a
	// turn_end, at prevTurnEnd. Only that event is read, so a turn whose
	// start lies before the caller's data is judged the same way.
	afterTurnEnd bool
	prevTurnEnd  time.Time
	// waits are the spans spent waiting on a permission ask; the agent isn't
	// busy then, the person is needed.
	waits []span
	// answers are the person's replies to permission asks: the session's
	// first event after an ask, with that event's project.
	answers []projectEvent
	// events are the turn's AI events that name a project, by time. They
	// name the project of the turn's agent minutes.
	events []projectEvent
	// automated: started by a prompt with no input before it (set by
	// markAutomated).
	automated bool
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

// ruleSpan is the part of the turn that a rule decided at its prompt covers:
// at most turnRuleReach after the prompt, so the decision is always made
// from data the agent still has when those minutes become final.
func (t turn) ruleSpan() span {
	end := t.end
	if limit := t.start.Add(turnRuleReach); end.After(limit) {
		end = limit
	}
	return span{t.start, end}
}

// projectAt returns the project of the turn's latest named event in
// [a, b), and its time; ok is false when there is none.
func (t turn) projectAt(a, b time.Time) (project string, at time.Time, ok bool) {
	i, _ := slices.BinarySearchFunc(t.events, b, func(e projectEvent, x time.Time) int { return e.t.Compare(x) })
	if i == 0 || t.events[i-1].t.Before(a) {
		return "", time.Time{}, false
	}
	return t.events[i-1].project, t.events[i-1].t, true
}

// sessionState tracks one AI session while events are replayed.
type sessionState struct {
	open      *turn
	last      time.Time
	lastKind  wire.AIEventKind
	waitFrom  time.Time
	waiting   bool
	completed []turn
}

func (s *sessionState) close(at time.Time) {
	if s.open == nil {
		return
	}
	if s.waiting {
		s.open.waits = append(s.open.waits, span{s.waitFrom, at})
		s.waiting = false
	}
	s.open.end = at
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
			st.close(st.last)
		}
		newAsk := e.kind == wire.AIPermissionAsk && !e.decided
		if st.waiting && !newAsk {
			// The person answered: the session moved on, or a reply arrived.
			st.open.waits = append(st.open.waits, span{st.waitFrom, e.t})
			st.open.answers = append(st.open.answers, projectEvent{e.t, e.project})
			st.waiting = false
		}
		switch {
		case e.kind == wire.AIPromptSubmit:
			st.close(e.t)
			st.open = &turn{session: e.session, start: e.t, prompt: true, chars: e.chars, hash: e.hash, promptProject: e.project}
			if st.lastKind == wire.AITurnEnd {
				st.open.afterTurnEnd, st.open.prevTurnEnd = true, st.last
			}
		case e.kind == wire.AIToolStart && st.open == nil:
			st.open = &turn{session: e.session, start: e.t}
		case newAsk && st.open == nil:
			// The agent is mid-turn and waiting. Without this, a wait whose
			// asks repeat for longer than ContextBefore would be seen, and
			// its answer recognised, only by callers holding its prompt.
			st.open = &turn{session: e.session, start: e.t}
			st.waitFrom, st.waiting = e.t, true
		case newAsk && st.open != nil && !st.waiting:
			st.waitFrom, st.waiting = e.t, true
		}
		if st.open != nil && e.project != "" {
			st.open.events = append(st.open.events, projectEvent{e.t, e.project})
		}
		if e.kind == wire.AITurnEnd || e.kind == wire.AISessionEnd {
			st.close(e.t)
		}
		st.last, st.lastKind = e.t, e.kind
	}
	var turns []turn
	slices.Sort(order)
	for _, id := range order {
		st := states[id]
		if st.open != nil {
			if horizon.Sub(st.last) > turnTimeout {
				st.close(st.last)
			} else {
				st.close(horizon)
			}
		}
		turns = append(turns, st.completed...)
	}
	slices.SortStableFunc(turns, func(a, b turn) int {
		return cmp.Or(a.start.Compare(b.start), strings.Compare(a.session, b.session))
	})
	return turns
}
