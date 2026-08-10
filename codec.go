// Zero-alloc ZAP-HTTP frame codec.
//
// This file is the hot-path encoder/decoder. It writes and reads the SAME
// bytes as the reference zap-proto/go Builder path (proven byte-identical by
// the golden-wire tests and TestCodec_CrossCheck), but without the per-request
// allocations that path incurred:
//
//   - no *zap.Builder / *zap.ObjectBuilder per marshal, no per-field double
//     copy (Builder.SetBytes did []byte(v) then append([]byte(nil), v...));
//   - no map[string][]string and no JSON for the headers slot — headers stream
//     straight into the frame's variable tail as length-prefixed pairs;
//   - no *zap.Message per parse and no map on decode — each name and value is a
//     subslice of the frame, applied straight to the fasthttp header store.
//
// Callers append into a reusable buffer (Append{Request,Response}), so after a
// connection warms up the steady-state cost is zero heap allocation.
//
// # Wire layout (see schema/zap_http.zap)
//
// A frame is one zap-proto/go message: a 16-byte header (magic "ZAP\x00",
// version=1, flags, rootOffset=16, size) followed by the root object. The root
// object is a fixed 48-byte section (fully zero-filled) whose text/bytes fields
// are 8-byte {relOffset,length} slots; every non-empty field's bytes are
// appended to the variable tail in field-declaration order and its slot's
// relOffset is patched to point at them. An empty text/bytes field stays a
// zeroed {0,0} slot with no tail entry. This is exactly what zap.Builder emits.

package zap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/valyala/fasthttp"
	zap "github.com/zap-proto/go"
)

// zeroFixed is a read-only source of zero bytes for laying down a struct's
// fixed section. Sized to the largest object (48 bytes); shared read-only
// across goroutines.
var zeroFixed [reqSlotSize]byte

// Static defaults, kept as byte slices so the encoder never allocates one.
var (
	bMethodGET    = []byte(fasthttp.MethodGet)
	bDefaultProto = []byte(defaultProto)
)

// ---- little-endian frame header ----

// appendFrameHeader writes the 16-byte ZAP message header with rootOffset=16
// (the root object begins immediately after the header, which is 8-aligned so
// the object needs no leading padding) and a placeholder size, patched by the
// caller once the frame is complete.
func appendFrameHeader(dst []byte, flags uint16) []byte {
	dst = append(dst, 'Z', 'A', 'P', 0)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(zap.Version))
	dst = binary.LittleEndian.AppendUint16(dst, flags)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(zap.HeaderSize)) // rootOffset
	dst = binary.LittleEndian.AppendUint32(dst, 0)                      // size (patched)
	return dst
}

// patchVarSlot patches an 8-byte {relOffset,length} slot to point at tail data
// that was already appended starting at dataStart. A zero-length field leaves
// the slot as the zeroed {0,0} null it started as (byte-identical to
// Builder.SetBytes(nil)).
func patchVarSlot(dst []byte, slot, dataStart int) []byte {
	n := len(dst) - dataStart
	if n == 0 {
		return dst
	}
	binary.LittleEndian.PutUint32(dst[slot:], uint32(dataStart-slot))
	binary.LittleEndian.PutUint32(dst[slot+4:], uint32(n))
	return dst
}

// putVarBytes appends a non-empty field's bytes to the tail and patches its
// slot. Empty data leaves the slot null.
func putVarBytes(dst []byte, slot int, data []byte) []byte {
	if len(data) == 0 {
		return dst
	}
	start := len(dst)
	dst = append(dst, data...)
	return patchVarSlot(dst, slot, start)
}

// ---- request / response encoders ----

// AppendRequest appends a request frame to dst and returns the extended slice.
// Passing a reused (len-0) buffer makes the steady-state marshal zero-alloc;
// MarshalRequest is the dst==nil convenience.
func AppendRequest(dst []byte, req *fasthttp.Request) ([]byte, error) {
	frameStart := len(dst)
	dst = appendFrameHeader(dst, FrameRequest<<8)
	obj := len(dst) // == frameStart + 16

	dst = append(dst, zeroFixed[:reqSlotSize]...)

	method := req.Header.Method()
	if len(method) == 0 {
		method = bMethodGET
	}
	proto := req.Header.Protocol()
	if len(proto) == 0 {
		proto = bDefaultProto
	}

	dst = putVarBytes(dst, obj+reqMethod, method)
	dst = putVarBytes(dst, obj+reqTarget, req.RequestURI())
	dst = putVarBytes(dst, obj+reqProto, proto)

	hStart := len(dst)
	skip := newTrailerSkip(&req.Header)
	dst = appendRequestHeaders(dst, &req.Header, skip)
	dst = patchVarSlot(dst, obj+reqHeaders, hStart)

	dst = putVarBytes(dst, obj+reqBody, req.Body())

	trailer, err := encodeTrailers(&req.Header)
	if err != nil {
		return dst, fmt.Errorf("zap: encode trailer: %w", err)
	}
	dst = putVarBytes(dst, obj+reqTrailer, trailer)

	binary.LittleEndian.PutUint32(dst[frameStart+12:], uint32(len(dst)-frameStart))
	return dst, nil
}

// AppendResponse appends a response frame to dst and returns the extended
// slice. See AppendRequest.
func AppendResponse(dst []byte, resp *fasthttp.Response) ([]byte, error) {
	frameStart := len(dst)
	dst = appendFrameHeader(dst, FrameResponse<<8)
	obj := len(dst)

	dst = append(dst, zeroFixed[:respSlotSize]...)

	status := resp.StatusCode()
	if status == 0 {
		status = fasthttp.StatusOK
	}
	binary.LittleEndian.PutUint16(dst[obj+respStatus:], uint16(status))

	reason := resp.Header.StatusMessage()
	if len(reason) == 0 {
		reason = unsafeBytes(fasthttp.StatusMessage(status))
	}
	proto := resp.Header.Protocol()
	if len(proto) == 0 {
		proto = bDefaultProto
	}

	dst = putVarBytes(dst, obj+respReason, reason)
	dst = putVarBytes(dst, obj+respProto, proto)

	hStart := len(dst)
	skip := newTrailerSkip(&resp.Header)
	dst = appendResponseHeaders(dst, &resp.Header, skip)
	dst = patchVarSlot(dst, obj+respHeaders, hStart)

	dst = putVarBytes(dst, obj+respBody, resp.Body())

	trailer, err := encodeTrailers(&resp.Header)
	if err != nil {
		return dst, fmt.Errorf("zap: encode trailer: %w", err)
	}
	dst = putVarBytes(dst, obj+respTrailer, trailer)

	binary.LittleEndian.PutUint32(dst[frameStart+12:], uint32(len(dst)-frameStart))
	return dst, nil
}

// ---- frame parsing (decode) ----

// frameRoot validates a frame's header (matching zap.Parse's magic/version/size
// checks) and confirms its type, returning the root object offset and the
// message length. Bytes past size are ignored, exactly as zap.Parse slices to
// data[:size].
func frameRoot(frame []byte, wantType uint16) (root, size int, err error) {
	if len(frame) < zap.HeaderSize {
		return 0, 0, zap.ErrBufferTooSmall
	}
	if string(frame[0:4]) != zap.Magic {
		return 0, 0, zap.ErrInvalidMagic
	}
	// Accept wire version 1 (this runtime's baseline) and 2 (the transport
	// framing generation); the data segment is identical across both.
	if v := binary.LittleEndian.Uint16(frame[4:6]); v != 1 && v != 2 {
		return 0, 0, zap.ErrInvalidVersion
	}
	size = int(binary.LittleEndian.Uint32(frame[12:16]))
	if size < zap.HeaderSize || size > len(frame) {
		return 0, 0, zap.ErrBufferTooSmall
	}
	if t := binary.LittleEndian.Uint16(frame[6:8]) >> 8; t != wantType {
		return 0, 0, fmt.Errorf("zap: frame type %#x, want %#x", t, wantType)
	}
	root = int(binary.LittleEndian.Uint32(frame[8:12]))
	if root < zap.HeaderSize || root >= size {
		return 0, 0, zap.ErrInvalidOffset
	}
	return root, size, nil
}

// frameType peeks a frame's type from its flags without a full parse. Zero
// alloc — the client uses it to dispatch streamed vs single-frame responses.
func frameType(frame []byte) (uint16, bool) {
	if len(frame) < zap.HeaderSize {
		return 0, false
	}
	return binary.LittleEndian.Uint16(frame[6:8]) >> 8, true
}

// readVar reads a text/bytes field at object offset fieldOff, zero-copy.
// Mirrors zap.Object.Bytes exactly, including the malleability guard that
// rejects a target inside the wire header (absPos < HeaderSize).
func readVar(frame []byte, size, root, fieldOff int) []byte {
	slot := root + fieldOff
	if slot+8 > size {
		return nil
	}
	rel := binary.LittleEndian.Uint32(frame[slot:])
	if rel == 0 {
		return nil
	}
	length := int(binary.LittleEndian.Uint32(frame[slot+4:]))
	abs := slot + int(rel)
	if abs < zap.HeaderSize || abs+length > size {
		return nil
	}
	return frame[abs : abs+length]
}

// readU16 reads a uint16 scalar at object offset fieldOff.
func readU16(frame []byte, size, root, fieldOff int) uint16 {
	pos := root + fieldOff
	if pos+2 > size {
		return 0
	}
	return binary.LittleEndian.Uint16(frame[pos:])
}

// ---- header encode ----

// hpair is one header name/value, both aliasing fasthttp's store; valid only
// for the duration of a single marshal call.
type hpair struct{ key, val []byte }

// hcollector is a pooled scratch slice for sorting header pairs.
type hcollector struct{ pairs []hpair }

var hcollectorPool = sync.Pool{New: func() any { return &hcollector{pairs: make([]hpair, 0, 16)} }}

// trailerSkip is the set of trailer-declared header names, which ride in the
// frame's trailer slot and must be excluded from the headers slot. nil when the
// message declares no trailers (the common case), so it costs nothing.
type trailerSkip struct{ keys [][]byte }

func newTrailerSkip(h interface{ PeekTrailerKeys() [][]byte }) *trailerSkip {
	keys := h.PeekTrailerKeys()
	if len(keys) == 0 {
		return nil
	}
	return &trailerSkip{keys: keys}
}

func (s *trailerSkip) has(key []byte) bool {
	if s == nil {
		return false
	}
	for _, k := range s.keys {
		if bytesEqualFold(k, key) {
			return true
		}
	}
	return false
}

// ---- headers ----
//
// Headers travel as a count-prefixed run of length-prefixed name/value pairs:
//
//	[u32 count]   then count times:   [u32 nameLen][name][u32 valueLen][value]
//
// Reading one is a walk over the frame's own bytes — every name and value is a
// subslice, so a decode allocates nothing. Repeated names are simply repeated
// pairs, which is what VisitAll already yields for a multi-value header, so
// nothing has to model a list. Order is preserved as sent, and there is no
// escaping, no sort, and no grammar for two implementations to disagree about.

// appendRequestHeaders and appendResponseHeaders collect a message's headers
// and emit them as length-prefixed pairs.
//
// Two concrete functions, not one generic over an interface, and that is NOT an
// oversight to be tidied away. A generic here compiles to a shared GC-shape
// instantiation (appendHeaders[go.shape.*uint8]), so the VisitAll call goes
// through a dictionary, escape analysis can no longer prove the closure stays
// on the stack, and every encode picks up a heap allocation — measured at
// exactly 1 allocs/op, 32 B/op, on a path documented and benchmarked as
// allocation-free. Duplicating twelve lines buys back zero-alloc encode.
func appendRequestHeaders(dst []byte, h *fasthttp.RequestHeader, skip *trailerSkip) []byte {
	c := hcollectorPool.Get().(*hcollector)
	c.pairs = c.pairs[:0]
	h.VisitAll(func(key, value []byte) {
		if isFrameOwnedHeaderBytes(key) || skip.has(key) {
			return
		}
		c.pairs = append(c.pairs, hpair{key, value})
	})
	dst = emitHeaders(dst, c.pairs)
	hcollectorPool.Put(c)
	return dst
}

func appendResponseHeaders(dst []byte, h *fasthttp.ResponseHeader, skip *trailerSkip) []byte {
	c := hcollectorPool.Get().(*hcollector)
	c.pairs = c.pairs[:0]
	h.VisitAll(func(key, value []byte) {
		if isFrameOwnedHeaderBytes(key) || skip.has(key) {
			return
		}
		c.pairs = append(c.pairs, hpair{key, value})
	})
	dst = emitHeaders(dst, c.pairs)
	hcollectorPool.Put(c)
	return dst
}

// emitHeaders writes pairs. No pairs appends nothing, leaving a null slot.
func emitHeaders(dst []byte, pairs []hpair) []byte {
	if len(pairs) == 0 {
		return dst
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(pairs)))
	for _, p := range pairs {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(p.key)))
		dst = append(dst, p.key...)
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(p.val)))
		dst = append(dst, p.val...)
	}
	return dst
}

// errHeaderShort reports a header block that ends mid-field.
var errHeaderShort = errors.New("zap: truncated header block")

// forEachHeader calls fn for every pair in raw. Keys and values alias raw and
// must not outlive it.
func forEachHeader(raw []byte, fn func(key, val []byte)) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) < 4 {
		return errHeaderShort
	}
	n := binary.LittleEndian.Uint32(raw)
	i := 4
	for ; n > 0; n-- {
		key, ni, err := headerField(raw, i)
		if err != nil {
			return err
		}
		val, ni2, err := headerField(raw, ni)
		if err != nil {
			return err
		}
		fn(key, val)
		i = ni2
	}
	return nil
}

// headerField reads one length-prefixed field starting at i.
func headerField(raw []byte, i int) ([]byte, int, error) {
	if i+4 > len(raw) {
		return nil, 0, errHeaderShort
	}
	n := int(binary.LittleEndian.Uint32(raw[i:]))
	i += 4
	if n < 0 || i+n > len(raw) {
		return nil, 0, errHeaderShort
	}
	return raw[i : i+n], i + n, nil
}

// ---- shared helpers ----

// isFrameOwnedHeaderBytes reports whether a header name is carried by the frame
// itself (Content-Length, from the body's own length prefix) or by the trailer
// slot (Trailer), and so is excluded from the headers slot. Byte-wise EqualFold,
// so it allocates nothing.
//
// HOST IS NOT ONE OF THEM, and used to be listed here. Nothing in the frame
// carries a host: the decoder reconstructs it from a Host HEADER
// (decodeRequestHeaders), and the encoder was dropping exactly that header. So
// every request over this wire arrived with an empty Host, and a callee serving
// more than one brand could not tell which one was asked for. It was invisible
// because a single-brand callee never reads it.
func isFrameOwnedHeaderBytes(key []byte) bool {
	return bytesEqualFold(key, strContentLength) ||
		bytesEqualFold(key, strTrailer)
}

var (
	strHost          = []byte(fasthttp.HeaderHost)
	strContentLength = []byte(fasthttp.HeaderContentLength)
	strTrailer       = []byte(fasthttp.HeaderTrailer)
)

// bytesEqualFold is ASCII case-insensitive byte equality (header names are
// ASCII tokens), allocation-free.
func bytesEqualFold(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca == cb {
			continue
		}
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// unsafeBytes returns the bytes of s without copying. The result is read-only;
// used only where the bytes are immediately copied out (append into a frame).
func unsafeBytes(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// unsafeString views b as a string without copying, for read-only use.
func unsafeString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}
