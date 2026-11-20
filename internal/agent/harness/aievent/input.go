package aievent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxPayload is the most stdin a hook command keeps in memory. Harness
// payloads can carry whole prompts or tool output; everything past this is
// read and thrown away.
const MaxPayload = 1 << 20

// maxDrain bounds how much extra stdin is discarded after the cap, so a
// misbehaving writer can't keep the hook running.
const maxDrain = 64 << 20

// Input is a hook payload read from stdin.
type Input struct {
	// Data holds at most MaxPayload bytes.
	Data []byte
	// Truncated means Data isn't the whole payload: stdin was longer than
	// MaxPayload, or didn't finish in time. Data is then a prefix (possibly
	// empty) and isn't valid JSON on its own.
	Truncated bool
}

// ReadInput reads r up to MaxPayload bytes, then discards (without keeping)
// up to maxDrain more so the harness's write doesn't fail on a closed pipe.
// It gives up when ctx is done and returns the prefix read so far: a harness
// that never closes stdin, or a slow machine, must not hang the hook, and the
// ids a parser needs come first in every payload.
func ReadInput(ctx context.Context, r io.Reader) Input {
	var (
		mu   sync.Mutex
		buf  []byte
		cut  bool
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		chunk := make([]byte, 32<<10)
		drained := 0
		for drained <= maxDrain {
			n, err := r.Read(chunk)
			mu.Lock()
			if room := MaxPayload - len(buf); n > room {
				buf = append(buf, chunk[:room]...)
				drained += n - room
				cut = true
			} else {
				buf = append(buf, chunk[:n]...)
			}
			mu.Unlock()
			if err != nil { // io.EOF or a broken pipe both end the payload
				return
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// The reader goroutine stays blocked on stdin until the process
		// exits, which happens right after the hook finishes.
	}
	mu.Lock()
	defer mu.Unlock()
	finished := false
	select {
	case <-done:
		finished = true
	default:
	}
	return Input{Data: bytes.Clone(buf), Truncated: cut || !finished}
}

// Salvage pulls the top-level string, number and boolean fields out of the
// start of a JSON object that was cut off, and re-encodes them as a small
// complete object. Nested values and the field that was cut are skipped, so
// a parser can still find ids like session_id even when a huge prompt or tool
// output made the payload too big to read whole. The parser's payload struct
// decides which of the salvaged fields are kept.
func Salvage(prefix []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return []byte("{}")
	}
	out := map[string]any{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := kt.(string)
		if !ok {
			break
		}
		if !skipOrTake(dec, key, out) {
			break
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// skipOrTake reads one value, storing it in out if it is a scalar. It
// returns false when the input ends mid-value.
func skipOrTake(dec *json.Decoder, key string, out map[string]any) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	if _, ok := tok.(json.Delim); ok {
		return skipNested(dec)
	}
	out[key] = tok
	return true
}

// skipNested consumes the rest of an object or array whose opening delimiter
// was already read.
func skipNested(dec *json.Decoder) bool {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return true
}

// Decode unmarshals a payload into v. For a truncated payload it decodes the
// salvaged top-level scalars instead. Fields of the wrong type are left zero
// rather than failing the whole event.
func Decode(in Input, v any) error {
	data := in.Data
	if in.Truncated {
		data = Salvage(in.Data)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	err := json.Unmarshal(data, v)
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decoding hook payload: %w", err)
	}
	return nil
}

// TextLen decodes a JSON string as its length in runes, so a parser can learn
// how long a prompt was without keeping its text. Non-strings decode as 0.
type TextLen int

// UnmarshalJSON implements json.Unmarshaler.
func (t *TextLen) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil {
		*t = 0
		return nil
	}
	*t = TextLen(Runes(s))
	return nil
}
