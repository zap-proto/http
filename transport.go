// Transport — length-prefixed framing over an io.ReadWriteCloser.
//
// v0.1 uses a 4-byte big-endian length prefix on each frame; v0.2
// will swap this for the full ZAP transport (X-Wing PQ KEM handshake
// + AEAD framing + multi-stream multiplexing) once zap-proto/go ships
// it. The marshal/unmarshal API in wire.go is stable across that
// change — only the bytes flowing on the socket change shape.
//
// Each ZAP-HTTP exchange is one request frame followed by one
// response frame on the same connection. Connections may be reused
// across exchanges (HTTP/1.1-style keep-alive); the transport itself
// does not pipeline.

package zaphttp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxFrameSize bounds an inbound frame. The wire format leaves room
// for larger frames; the limit here defends a server against a
// malicious peer announcing a multi-gigabyte length prefix.
const MaxFrameSize = 64 << 20 // 64 MiB

// readFrame reads one length-prefixed frame from r into a freshly allocated
// slice. nil + io.EOF indicates the peer closed cleanly between frames. Hot
// paths use readFrameInto to reuse a buffer instead.
func readFrame(r *bufio.Reader) ([]byte, error) {
	return readFrameInto(r, nil)
}

// readFrameInto reads one length-prefixed frame into buf, growing it only when
// the frame exceeds its capacity, and returns the frame slice (buf reused). The
// caller reassigns its buffer from the return value so a grown buffer persists
// across calls — the steady-state read is allocation-free. The frame's bytes
// are consumed synchronously by Unmarshal{Request,Response} (which copy into the
// fasthttp store), so the buffer is free to reuse on the next call.
func readFrameInto(r *bufio.Reader, buf []byte) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return buf, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return buf, fmt.Errorf("zaphttp: zero-length frame")
	}
	if n > MaxFrameSize {
		return buf, fmt.Errorf("zaphttp: frame size %d exceeds MaxFrameSize=%d", n, MaxFrameSize)
	}
	if uint32(cap(buf)) < n {
		buf = make([]byte, n)
	} else {
		buf = buf[:n]
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return buf, fmt.Errorf("zaphttp: short frame: %w", err)
	}
	return buf, nil
}

// writeFramePrefixed writes a length prefix + frame in ONE Write. The frame
// buffer must have been built with 4 leading bytes reserved for the prefix
// (frame[:4] is the placeholder); this halves the per-message syscall count vs
// writeFrame's separate header+body writes.
func writeFramePrefixed(w io.Writer, framePlusPrefix []byte) error {
	body := len(framePlusPrefix) - 4
	if body <= 0 {
		return fmt.Errorf("zaphttp: refusing to write zero-length frame")
	}
	if uint64(body) > MaxFrameSize {
		return fmt.Errorf("zaphttp: frame size %d exceeds MaxFrameSize=%d", body, MaxFrameSize)
	}
	binary.BigEndian.PutUint32(framePlusPrefix[0:4], uint32(body))
	_, err := w.Write(framePlusPrefix)
	return err
}

// writeFrame writes one length-prefixed frame to w.
func writeFrame(w io.Writer, frame []byte) error {
	if len(frame) == 0 {
		return fmt.Errorf("zaphttp: refusing to write zero-length frame")
	}
	if uint64(len(frame)) > MaxFrameSize {
		return fmt.Errorf("zaphttp: frame size %d exceeds MaxFrameSize=%d", len(frame), MaxFrameSize)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(frame)
	return err
}
