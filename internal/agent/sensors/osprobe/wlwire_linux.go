//go:build linux

package osprobe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A minimal Wayland wire-protocol codec, just enough for ext-idle-notify-v1:
// uint/int/object/new_id arguments and strings. File descriptors and arrays
// are never sent or needed. See
// https://wayland.freedesktop.org/docs/html/ch04.html#sect-Protocol-Wire-Format
//
// A message is a header of two 32-bit words in the host's byte order (the
// object id, then size<<16 | opcode, size including the header) followed by
// 32-bit aligned arguments.

// wlMaxMessage is the largest message the wire format can express.
const wlMaxMessage = 1<<16 - 1

var errWlMalformed = errors.New("wayland: malformed message")

// wlMessage is one decoded message.
type wlMessage struct {
	object uint32
	opcode uint16
	args   []byte
}

// wlBuild appends arguments to a message and encodes it.
type wlBuild struct {
	buf []byte
}

func (b *wlBuild) uint(v uint32) *wlBuild {
	b.buf = binary.NativeEndian.AppendUint32(b.buf, v)
	return b
}

// str appends a string: its length including the NUL terminator, the bytes,
// the NUL, then padding to 32 bits.
func (b *wlBuild) str(s string) *wlBuild {
	b.uint(uint32(len(s) + 1)) //nolint:gosec // only short constant interface names are sent
	b.buf = append(b.buf, s...)
	b.buf = append(b.buf, 0)
	for len(b.buf)%4 != 0 {
		b.buf = append(b.buf, 0)
	}
	return b
}

// message returns the encoded message for object and opcode.
func (b *wlBuild) message(object uint32, opcode uint16) []byte {
	size := 8 + len(b.buf)
	out := make([]byte, 0, size)
	out = binary.NativeEndian.AppendUint32(out, object)
	out = binary.NativeEndian.AppendUint32(out, uint32(size)<<16|uint32(opcode)) //nolint:gosec // our requests are a few dozen bytes
	return append(out, b.buf...)
}

// wlRead reads one message from r into buf (which must hold wlMaxMessage
// bytes) and returns it; args alias buf.
func wlRead(r io.Reader, buf []byte) (wlMessage, error) {
	if _, err := io.ReadFull(r, buf[:8]); err != nil {
		return wlMessage{}, err
	}
	object := binary.NativeEndian.Uint32(buf[0:4])
	word := binary.NativeEndian.Uint32(buf[4:8])
	size := int(word >> 16)
	if size < 8 || size%4 != 0 {
		return wlMessage{}, fmt.Errorf("%w: size %d", errWlMalformed, size)
	}
	if _, err := io.ReadFull(r, buf[8:size]); err != nil {
		return wlMessage{}, err
	}
	return wlMessage{object: object, opcode: uint16(word & 0xffff), args: buf[8:size]}, nil
}

// wlArgs decodes arguments in order. The first decoding error sticks.
type wlArgs struct {
	b   []byte
	err error
}

func (a *wlArgs) uint() uint32 {
	if a.err != nil {
		return 0
	}
	if len(a.b) < 4 {
		a.err = errWlMalformed
		return 0
	}
	v := binary.NativeEndian.Uint32(a.b)
	a.b = a.b[4:]
	return v
}

func (a *wlArgs) str() string {
	n := int(a.uint())
	if a.err != nil {
		return ""
	}
	padded := (n + 3) &^ 3
	if n == 0 || padded > len(a.b) || a.b[n-1] != 0 {
		a.err = errWlMalformed
		return ""
	}
	s := string(a.b[:n-1])
	a.b = a.b[padded:]
	return s
}
