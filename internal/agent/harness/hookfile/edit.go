package hookfile

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/apneduniya/logbook/internal/agent/harness/jsonedit"
)

// Layout is how a harness nests hook handlers under "hooks".
type Layout int

const (
	// Nested is Claude Code's shape, also used by Codex and Gemini CLI:
	// {"hooks": {"Event": [{"matcher": "...", "hooks": [handler, ...]}]}}.
	Nested Layout = iota
	// Flat is Cursor's and Copilot's shape:
	// {"version": 1, "hooks": {"event": [handler, ...]}}.
	Flat
)

// Entry is one Logbook hook to install.
type Entry struct {
	Event string
	// Matcher is the matcher-group filter for Nested layouts ("" = all). Flat
	// layouts put any matcher inside Handler.
	Matcher string
	// Handler is the handler object, exactly as it should appear in the file.
	Handler json.RawMessage
}

// ArgvFunc extracts the command a handler runs, as argv. It returns nil for
// handlers that don't run a command (HTTP, prompt, MCP and so on).
type ArgvFunc func(handler json.RawMessage) []string

// editor applies Logbook's entries to the "hooks" object of a config file.
type editor struct {
	layout Layout
	ours   func(handler json.RawMessage) bool
}

// found is one Logbook handler located in the file.
type found struct {
	matcher string
	handler json.RawMessage
}

func (e editor) hooksObject(doc *jsonedit.Object) (*jsonedit.Object, error) {
	raw, ok := doc.Get("hooks")
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return jsonedit.NewObject(), nil
	}
	hooks, err := jsonedit.ParseObject(raw)
	if err != nil {
		return nil, fmt.Errorf(`"hooks" is not an object: %w`, err)
	}
	return hooks, nil
}

// apply makes the file's Logbook entries equal to want, leaving every other
// entry alone. It reports whether anything changed.
func (e editor) apply(doc *jsonedit.Object, want []Entry) (bool, error) {
	hooks, err := e.hooksObject(doc)
	if err != nil {
		return false, err
	}
	byEvent := map[string][]Entry{}
	var order []string
	for _, w := range want {
		if _, seen := byEvent[w.Event]; !seen {
			order = append(order, w.Event)
		}
		byEvent[w.Event] = append(byEvent[w.Event], w)
	}
	changed := false
	// Events Logbook no longer installs (from an older version) lose their
	// Logbook entries.
	for _, event := range hooks.Keys() {
		if _, keep := byEvent[event]; keep {
			continue
		}
		c, err := e.removeFromEvent(hooks, event)
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	for _, event := range order {
		c, err := e.setEvent(hooks, event, byEvent[event])
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	if changed {
		out, err := hooks.MarshalJSON()
		if err != nil {
			return false, err
		}
		doc.Set("hooks", out)
	}
	return changed, nil
}

// remove deletes every Logbook entry. It reports whether anything changed.
// An emptied "hooks" object is left in place; Installer.tidy decides whether
// it goes.
func (e editor) remove(doc *jsonedit.Object) (bool, error) {
	if _, ok := doc.Get("hooks"); !ok {
		return false, nil
	}
	hooks, err := e.hooksObject(doc)
	if err != nil {
		return false, err
	}
	changed := false
	for _, event := range hooks.Keys() {
		c, err := e.removeFromEvent(hooks, event)
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	if !changed {
		return false, nil
	}
	out, err := hooks.MarshalJSON()
	if err != nil {
		return false, err
	}
	doc.Set("hooks", out)
	return true, nil
}

// count returns how many Logbook entries the file has.
func (e editor) count(doc *jsonedit.Object) (int, error) {
	if _, ok := doc.Get("hooks"); !ok {
		return 0, nil
	}
	hooks, err := e.hooksObject(doc)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, event := range hooks.Keys() {
		elems, err := eventArray(hooks, event)
		if err != nil {
			return 0, err
		}
		got, _, err := e.split(elems)
		if err != nil {
			return 0, err
		}
		n += len(got)
	}
	return n, nil
}

func eventArray(hooks *jsonedit.Object, event string) ([]json.RawMessage, error) {
	raw, ok := hooks.Get(event)
	if !ok {
		return nil, nil
	}
	elems, err := jsonedit.Array(raw)
	if err != nil {
		return nil, fmt.Errorf("hooks.%s: %w", event, err)
	}
	return elems, nil
}

// setEvent makes event's Logbook entries equal to want.
func (e editor) setEvent(hooks *jsonedit.Object, event string, want []Entry) (bool, error) {
	elems, err := eventArray(hooks, event)
	if err != nil {
		return false, err
	}
	got, rest, err := e.split(elems)
	if err != nil {
		return false, err
	}
	if sameEntries(got, want) {
		return false, nil
	}
	for _, w := range want {
		rest = append(rest, e.element(w))
	}
	hooks.Set(event, jsonedit.MarshalArray(rest))
	return true, nil
}

// removeFromEvent drops event's Logbook entries, and the event itself if
// nothing else is left in it.
func (e editor) removeFromEvent(hooks *jsonedit.Object, event string) (bool, error) {
	elems, err := eventArray(hooks, event)
	if err != nil {
		return false, err
	}
	got, rest, err := e.split(elems)
	if err != nil {
		return false, err
	}
	if len(got) == 0 {
		return false, nil
	}
	if len(rest) == 0 {
		hooks.Delete(event)
	} else {
		hooks.Set(event, jsonedit.MarshalArray(rest))
	}
	return true, nil
}

func sameEntries(got []found, want []Entry) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].matcher != want[i].Matcher || !jsonedit.Equal(got[i].handler, want[i].Handler) {
			return false
		}
	}
	return true
}

// element is the array element an Entry becomes.
func (e editor) element(w Entry) json.RawMessage {
	if e.layout == Flat {
		return w.Handler
	}
	g := jsonedit.NewObject()
	if w.Matcher != "" {
		m, _ := json.Marshal(w.Matcher) // marshalling a string can't fail
		g.Set("matcher", m)
	}
	g.Set("hooks", jsonedit.MarshalArray([]json.RawMessage{w.Handler}))
	out, _ := g.MarshalJSON() // values above are valid JSON
	return out
}

// split separates an event's elements into Logbook's handlers and everything
// else. In the nested layout a group that mixes Logbook's handler with others
// keeps the others; a group holding only Logbook's handlers is dropped.
// Elements that aren't Logbook's are returned byte for byte.
func (e editor) split(elems []json.RawMessage) ([]found, []json.RawMessage, error) {
	var got []found
	var rest []json.RawMessage
	for _, el := range elems {
		if e.layout == Flat {
			if e.ours(el) {
				got = append(got, found{handler: el})
			} else {
				rest = append(rest, el)
			}
			continue
		}
		group, err := jsonedit.ParseObject(el)
		if err != nil {
			rest = append(rest, el) // not a matcher group; not ours to judge
			continue
		}
		raw, ok := group.Get("hooks")
		if !ok {
			rest = append(rest, el)
			continue
		}
		handlers, err := jsonedit.Array(raw)
		if err != nil {
			rest = append(rest, el)
			continue
		}
		matcher := groupMatcher(group)
		var keep []json.RawMessage
		for _, h := range handlers {
			if e.ours(h) {
				got = append(got, found{matcher: matcher, handler: h})
			} else {
				keep = append(keep, h)
			}
		}
		switch {
		case len(keep) == len(handlers):
			rest = append(rest, el)
		case len(keep) > 0:
			group.Set("hooks", jsonedit.MarshalArray(keep))
			out, err := group.MarshalJSON()
			if err != nil {
				return nil, nil, err
			}
			rest = append(rest, out)
		}
	}
	return got, rest, nil
}

func groupMatcher(group *jsonedit.Object) string {
	raw, ok := group.Get("matcher")
	if !ok {
		return ""
	}
	var m string
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m
}
