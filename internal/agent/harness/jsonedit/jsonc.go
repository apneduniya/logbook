package jsonedit

import "bytes"

// StripJSONC turns JSONC into plain JSON: it removes // and /* */ comments
// and trailing commas before ] and }, leaving string contents alone. It also
// drops a leading UTF-8 byte order mark. Comments and commas are replaced by
// spaces, so after the byte order mark every byte keeps its offset. The
// second result reports whether anything other than whitespace was removed.
func StripJSONC(b []byte) ([]byte, bool) {
	return strip(b, true)
}

// stripComments is StripJSONC without removing trailing commas.
func stripComments(b []byte) []byte {
	out, _ := strip(b, false)
	return out
}

func strip(b []byte, commas bool) ([]byte, bool) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	out := make([]byte, 0, len(b))
	changed := false
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			out = append(out, c)
			switch c {
			case '\\':
				if i+1 < len(b) {
					i++
					out = append(out, b[i])
				}
			case '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			changed = true
			for i < len(b) && b[i] != '\n' {
				out = append(out, ' ')
				i++
			}
			if i < len(b) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			changed = true
			out = append(out, ' ', ' ')
			i += 2
			for i < len(b) && (b[i] != '*' || i+1 >= len(b) || b[i+1] != '/') {
				if b[i] == '\n' {
					out = append(out, '\n')
				} else {
					out = append(out, ' ')
				}
				i++
			}
			if i < len(b) { // skip the closing "*/"
				i++
				out = append(out, ' ', ' ')
			}
		case commas && c == ',' && nextSignificant(b, i+1) != 0 && (nextSignificant(b, i+1) == ']' || nextSignificant(b, i+1) == '}'):
			changed = true
			out = append(out, ' ')
		default:
			out = append(out, c)
		}
	}
	return out, changed
}

// nextSignificant returns the next byte after b[from:] that isn't whitespace
// or inside a comment, or 0 at the end of input.
func nextSignificant(b []byte, from int) byte {
	for i := from; i < len(b); i++ {
		switch c := b[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			continue
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i < len(b) && (b[i] != '*' || i+1 >= len(b) || b[i+1] != '/') {
				i++
			}
			i++
		default:
			return c
		}
	}
	return 0
}
