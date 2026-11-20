package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrDuplicateKey means the object has the member to splice more than once,
// so a byte-level edit can't say which copy the reader uses.
var ErrDuplicateKey = errors.New("key appears more than once")

const bom = "\xef\xbb\xbf"

// span is where one top-level member sits in the source.
type span struct {
	keyStart, valStart, valEnd int
}

// layout is the position of a top-level object's members in a source.
type layout struct {
	open, close int // offsets of '{' and '}'
	members     map[string][]span
	firstKey    int // offset of the first key, or -1
}

// scan locates the top-level members of src, which must be plain JSON with
// the same byte offsets as the original (StripJSONC keeps them).
func scan(plain []byte) (layout, error) {
	l := layout{members: map[string][]span{}, firstKey: -1}
	dec := json.NewDecoder(bytes.NewReader(plain))
	tok, err := dec.Token()
	if err != nil {
		return l, fmt.Errorf("parsing JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return l, ErrNotObject
	}
	l.open = int(dec.InputOffset()) - 1
	for dec.More() {
		prev := int(dec.InputOffset())
		kt, err := dec.Token()
		if err != nil {
			return l, fmt.Errorf("parsing JSON key: %w", err)
		}
		key, ok := kt.(string)
		if !ok {
			return l, fmt.Errorf("parsing JSON: unexpected token %v", kt)
		}
		keyStart := bytes.IndexByte(plain[prev:], '"') + prev
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return l, fmt.Errorf("parsing JSON value for %q: %w", key, err)
		}
		end := int(dec.InputOffset())
		l.members[key] = append(l.members[key], span{keyStart: keyStart, valStart: end - len(raw), valEnd: end})
		if l.firstKey < 0 {
			l.firstKey = keyStart
		}
	}
	if _, err := dec.Token(); err != nil {
		return l, fmt.Errorf("parsing JSON: %w", err)
	}
	l.close = int(dec.InputOffset()) - 1
	return l, nil
}

// SetMember returns src, a JSON or JSONC object, with its top-level member
// key set to val, or removed when val is nil. Every other byte, comments
// included, is kept; the new value is indented to match its line. It fails
// when src isn't an object or has key more than once.
func SetMember(src []byte, key string, val json.RawMessage) ([]byte, error) {
	lead := 0
	if bytes.HasPrefix(src, []byte(bom)) {
		lead = len(bom)
	}
	body := src[lead:]
	plain, _ := StripJSONC(body) // same length and offsets as body
	l, err := scan(plain)
	if err != nil {
		return nil, err
	}
	spans := l.members[key]
	if len(spans) > 1 {
		return nil, fmt.Errorf("%q: %w", key, ErrDuplicateKey)
	}
	// bare has comments blanked but keeps trailing commas, for finding the
	// commas around a member.
	bare := stripComments(body)
	var out []byte
	switch {
	case len(spans) == 1 && val != nil:
		out, err = replaceValue(body, spans[0], val)
	case len(spans) == 1:
		out = removeMember(body, bare, spans[0])
	case val != nil:
		out, err = insertMember(body, bare, l, key, val)
	default:
		out = body
	}
	if err != nil {
		return nil, err
	}
	res := make([]byte, 0, lead+len(out))
	res = append(res, src[:lead]...)
	return append(res, out...), nil
}

func replaceValue(body []byte, s span, val json.RawMessage) ([]byte, error) {
	v, err := indent(val, lineIndent(body, s.keyStart))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(body)+len(v))
	out = append(out, body[:s.valStart]...)
	out = append(out, v...)
	return append(out, body[s.valEnd:]...), nil
}

// removeMember cuts the member and one comma next to it: the one after it,
// or, for the last member, the one before it.
func removeMember(body, bare []byte, s span) []byte {
	start, end := s.keyStart, s.valEnd
	if next := nextNonSpace(bare, end); next < len(bare) && bare[next] == ',' {
		end = next + 1
	} else if prev := prevNonSpace(bare, start); prev >= 0 && bare[prev] == ',' {
		// Drop the preceding comma separately, so a comment between it and
		// the member stays.
		out := make([]byte, 0, len(body))
		out = append(out, body[:prev]...)
		body = append(out, body[prev+1:]...)
		start--
		end--
	}
	// Take the whole line when the member had it to itself.
	ls := lineStart(body, start)
	le := end
	for le < len(body) && (body[le] == ' ' || body[le] == '\t' || body[le] == '\r') {
		le++
	}
	if isBlank(body[ls:start]) && le < len(body) && body[le] == '\n' {
		start, end = ls, le+1
	}
	out := make([]byte, 0, len(body))
	out = append(out, body[:start]...)
	return append(out, body[end:]...)
}

func insertMember(body, bare []byte, l layout, key string, val json.RawMessage) ([]byte, error) {
	// Indent like the first member, when it has a line of its own.
	ind := "  "
	if l.firstKey >= 0 && lineStart(body, l.firstKey) > l.open {
		ind = lineIndent(body, l.firstKey)
	}
	k, err := json.Marshal(key)
	if err != nil {
		return nil, fmt.Errorf("encoding key: %w", err)
	}
	v, err := indent(val, ind)
	if err != nil {
		return nil, err
	}
	member := ind + string(k) + ": " + string(v) + "\n"
	// The member goes just above a closing brace that has its own line, or
	// on a new line before it otherwise.
	at := l.close
	last := prevNonSpace(bare, l.close)
	if ls := lineStart(body, l.close); isBlank(body[ls:l.close]) && ls > last {
		at = ls
	} else {
		member = "\n" + member
	}
	out := make([]byte, 0, len(body)+len(member)+1)
	out = append(out, body[:last+1]...)
	// The previous member (if any) needs a comma, unless it already has a
	// trailing one. Anything after it, such as a line comment, is kept.
	if last != l.open && bare[last] != ',' {
		out = append(out, ',')
	}
	out = append(out, body[last+1:at]...)
	out = append(out, member...)
	return append(out, body[at:]...), nil
}

func indent(val json.RawMessage, prefix string) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, val, prefix, "  "); err != nil {
		return nil, fmt.Errorf("indenting JSON: %w", err)
	}
	return buf.Bytes(), nil
}

func lineStart(b []byte, i int) int {
	return bytes.LastIndexByte(b[:i], '\n') + 1
}

// lineIndent returns the spaces and tabs that start the line holding b[i].
func lineIndent(b []byte, i int) string {
	ls := lineStart(b, i)
	end := ls
	for end < i && (b[end] == ' ' || b[end] == '\t') {
		end++
	}
	return string(b[ls:end])
}

func isBlank(b []byte) bool { return len(bytes.TrimSpace(b)) == 0 }

func nextNonSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

func prevNonSpace(b []byte, i int) int {
	i--
	for i >= 0 && isSpace(b[i]) {
		i--
	}
	return i
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
