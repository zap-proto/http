// Package http implements HTTP request/response semantics over the
// ZAP transport for the fasthttp handler model. A fasthttp.RequestHandler
// is served over ZAP frames; a fasthttp.Request/Response pair is exchanged
// by the client. There is no net/http in the request path — Fiber and
// every other fasthttp stack hand their handler straight to this package.
//
// The wire format is documented in schema/http.zap (authored in the
// ZAP schema language). The bytes on the wire are github.com/zap-proto/go
// Objects — the canonical pure-stdlib ZAP runtime — packed at explicit
// byte offsets within each object's fixed payload. There is no external
// codec dependency: codec.go is the encoder and decoder.
//
// # Wire
//
// Object slots, offsets and the flags discriminator are stable. Headers moved
// from a JSON map to length-prefixed pairs — a JSON encoder inside a zero-copy
// binary protocol was buying escaping, sorting and a grammar nobody needed, and
// it forced two implementations to be kept byte-identical. The golden-wire
// tests pin the current bytes; they were regenerated once, deliberately, for
// that change. Peers must agree on the version.
//
// What's covered:
//   - Request method, target, headers, body
//   - Response status, reason, headers, body
//   - Trailers (read and write)
//
// What's not yet covered:
//   - Streaming bodies larger than one frame use the head/data/end frames
//     below; a non-streamed body must fit in one frame (transport-negotiated
//     max frame size).
//   - WebSocket / SSE upgrade paths; those land in zap-proto/ws.
//
// # Wire layout
//
// A frame is one zap-proto/go message. The message type rides in the header
// flags (flags>>8): FrameRequest for a request, FrameResponse for a response,
// and the streaming variants below. Every text/bytes field is an 8-byte
// {relOffset,length} slot in the object's fixed section; non-empty fields'
// bytes are appended to the variable tail in field-declaration order. See
// codec.go for the encoder/decoder and the offset discipline that keeps the
// bytes byte-identical.
//
// # Framing metadata vs headers
//
// Three fields are owned by the frame, not the headers slot: Content-Length
// (the frame length-prefixes the body), Host (fasthttp's Host slot), and
// Trailer (the dedicated trailer slot). Everything else rides in the headers
// slot as length-prefixed name/value pairs. A repeated name is simply a
// repeated pair, which covers multi-value headers (RFC 9110 §5.2) without
// modelling a list.

package http

import (
	"encoding/binary"
	"fmt"
	"net/textproto"

	"github.com/valyala/fasthttp"
	zap "github.com/zap-proto/go"
)

// Frame type IDs. The ZAP message header carries the type in the upper byte
// of the 16-bit flags field; the encoder tags the message with type<<8 and
// flags>>8 recovers it. A request frame and a response frame are the two
// shapes the non-streaming wire carries.
const (
	FrameRequest  uint16 = 0x01
	FrameResponse uint16 = 0x02
	// Streaming response frames. A streamed response is a FrameResponseHead
	// (status + headers, no body) followed by zero or more FrameData chunks and
	// a terminating FrameEnd (optional trailers). This is how server→client
	// push (SSE, MCP notifications, chunked bodies) rides ZAP — the analogue of
	// HTTP/2 HEADERS + DATA + END_STREAM. The non-streaming FrameResponse path
	// is unchanged, so existing peers interoperate.
	FrameResponseHead uint16 = 0x03
	FrameData         uint16 = 0x04
	FrameEnd          uint16 = 0x05
)

// Data/End frame layout: a single bytes slot (the chunk, or the trailer pairs).
const (
	chunkBytes    = 0 // bytes (8)
	chunkSlotSize = 8
)

// Request frame field offsets within the object's fixed payload. Every
// text/bytes slot is 8 bytes (relOffset+length); see codec.go's offset
// discipline.
const (
	reqMethod   = 0  // text  (8)
	reqTarget   = 8  // text  (8)
	reqProto    = 16 // text  (8)
	reqHeaders  = 24 // bytes (8) -- name/value pairs
	reqBody     = 32 // bytes (8)
	reqTrailer  = 40 // bytes (8) -- name/value pairs
	reqSlotSize = 48
)

// Response frame field offsets. status@0 is a uint16 (2 bytes); the first text
// slot starts at the next 8-byte boundary.
const (
	respStatus   = 0  // uint16 (2)
	respReason   = 8  // text   (8)
	respProto    = 16 // text   (8)
	respHeaders  = 24 // bytes  (8) -- name/value pairs
	respBody     = 32 // bytes  (8)
	respTrailer  = 40 // bytes  (8) -- name/value pairs
	respSlotSize = 48
)

// defaultProto is the proto string written when the source carries none.
// fasthttp always reports a protocol ("HTTP/1.1" by default), so this only
// guards a hand-built zero-value header.
const defaultProto = "ZAP-HTTP/1.0"

// FrameTypeOf peeks a frame's type from its ZAP header without decoding the
// body, so a reader can dispatch (response vs streamed head vs data vs end).
func FrameTypeOf(frame []byte) (uint16, error) {
	if len(frame) < zap.HeaderSize {
		return 0, zap.ErrBufferTooSmall
	}
	if string(frame[0:4]) != zap.Magic {
		return 0, zap.ErrInvalidMagic
	}
	return binary.LittleEndian.Uint16(frame[6:8]) >> 8, nil
}

// MarshalRequest serializes a *fasthttp.Request into a ZAP frame. The returned
// bytes are the ZAP message; the transport layer prepends the length prefix
// (see transport.go). It is the dst==nil convenience for AppendRequest — hot
// callers append into a reused buffer to stay allocation-free.
func MarshalRequest(req *fasthttp.Request) ([]byte, error) {
	return AppendRequest(make([]byte, 0, 128+len(req.Body())), req)
}

// MarshalResponse serializes a *fasthttp.Response into a ZAP frame. See
// MarshalRequest; AppendResponse is the reused-buffer form.
func MarshalResponse(resp *fasthttp.Response) ([]byte, error) {
	return AppendResponse(make([]byte, 0, 128+len(resp.Body())), resp)
}

// UnmarshalRequest reconstructs a request into dst from a ZAP frame. dst is
// reset first, then populated: method, request-URI, protocol, headers,
// trailers, and body. Content-Length is derived from the body length by
// SetBody; Host comes from a Host header if the frame carried one.
func UnmarshalRequest(frame []byte, dst *fasthttp.Request) error {
	root, size, err := frameRoot(frame, FrameRequest)
	if err != nil {
		return err
	}
	method := readVar(frame, size, root, reqMethod)
	target := readVar(frame, size, root, reqTarget)
	proto := readVar(frame, size, root, reqProto)
	headers := readVar(frame, size, root, reqHeaders)
	body := readVar(frame, size, root, reqBody)
	trailer := readVar(frame, size, root, reqTrailer)

	dst.Reset()
	if len(method) == 0 {
		method = bMethodGET
	}
	dst.Header.SetMethodBytes(method)
	dst.SetRequestURIBytes(target)
	if len(proto) != 0 {
		dst.Header.SetProtocolBytes(proto)
	}
	if err := decodeRequestHeaders(&dst.Header, headers); err != nil {
		return fmt.Errorf("http: decode headers: %w", err)
	}
	if err := applyTrailers(&dst.Header, trailer); err != nil {
		return fmt.Errorf("http: decode trailer: %w", err)
	}
	dst.SetBody(body)
	return nil
}

// UnmarshalResponse reconstructs a response into dst from a ZAP frame.
func UnmarshalResponse(frame []byte, dst *fasthttp.Response) error {
	root, size, err := frameRoot(frame, FrameResponse)
	if err != nil {
		return err
	}
	status := int(readU16(frame, size, root, respStatus))
	reason := readVar(frame, size, root, respReason)
	proto := readVar(frame, size, root, respProto)
	headers := readVar(frame, size, root, respHeaders)
	body := readVar(frame, size, root, respBody)
	trailer := readVar(frame, size, root, respTrailer)

	dst.Reset()
	dst.SetStatusCode(status)
	if len(reason) != 0 {
		dst.Header.SetStatusMessage(reason)
	}
	if len(proto) != 0 {
		dst.Header.SetProtocol(proto)
	}
	if err := decodeResponseHeaders(&dst.Header, headers); err != nil {
		return fmt.Errorf("http: decode headers: %w", err)
	}
	if err := applyTrailers(&dst.Header, trailer); err != nil {
		return fmt.Errorf("http: decode trailer: %w", err)
	}
	dst.SetBody(body)
	return nil
}

// ---- streaming frames ----

// MarshalResponseHead serializes a response's status + headers (NO body) as the
// opening frame of a stream. The body is delivered by subsequent FrameData
// frames.
func MarshalResponseHead(resp *fasthttp.Response) ([]byte, error) {
	dst := appendFrameHeader(nil, FrameResponseHead<<8)
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
	dst = appendResponseHeaders(dst, &resp.Header, newTrailerSkip(&resp.Header))
	dst = patchVarSlot(dst, obj+respHeaders, hStart)
	// body and trailer slots stay null.

	binary.LittleEndian.PutUint32(dst[12:], uint32(len(dst)))
	return dst, nil
}

// UnmarshalResponseHead applies a streamed response's status + headers to dst.
// The body is left empty; the caller attaches a streaming body that reads the
// following FrameData frames.
func UnmarshalResponseHead(frame []byte, dst *fasthttp.Response) error {
	root, size, err := frameRoot(frame, FrameResponseHead)
	if err != nil {
		return err
	}
	dst.Reset()
	dst.SetStatusCode(int(readU16(frame, size, root, respStatus)))
	if reason := readVar(frame, size, root, respReason); len(reason) != 0 {
		dst.Header.SetStatusMessage(reason)
	}
	if proto := readVar(frame, size, root, respProto); len(proto) != 0 {
		dst.Header.SetProtocol(proto)
	}
	return decodeResponseHeaders(&dst.Header, readVar(frame, size, root, respHeaders))
}

// MarshalData wraps one body chunk as a FrameData frame.
func MarshalData(chunk []byte) []byte {
	return marshalChunk(FrameData, chunk)
}

// MarshalEnd terminates a stream, carrying optional encoded trailers.
func MarshalEnd(trailer []byte) []byte {
	return marshalChunk(FrameEnd, trailer)
}

// marshalChunk builds a single-bytes-field frame (data or end).
func marshalChunk(frameType uint16, data []byte) []byte {
	dst := appendFrameHeader(nil, frameType<<8)
	obj := len(dst)
	dst = append(dst, zeroFixed[:chunkSlotSize]...)
	dst = putVarBytes(dst, obj+chunkBytes, data)
	binary.LittleEndian.PutUint32(dst[12:], uint32(len(dst)))
	return dst
}

// DataChunkOf extracts the chunk bytes from a FrameData frame. The returned
// slice aliases the frame buffer; copy it to retain past the frame's lifetime.
func DataChunkOf(frame []byte) ([]byte, error) {
	root, size, err := frameRoot(frame, FrameData)
	if err != nil {
		return nil, err
	}
	return readVar(frame, size, root, chunkBytes), nil
}

// ---- header decode ----

// decodeRequestHeaders applies a request's headers to h. Host and
// Content-Length are frame-owned: Host is reconstructed here, Content-Length is
// set authoritatively by SetBody.
func decodeRequestHeaders(h *fasthttp.RequestHeader, raw []byte) error {
	return forEachHeader(raw, func(key, val []byte) {
		switch {
		case bytesEqualFold(key, strHost):
			h.SetHostBytes(val) // last value wins
		case bytesEqualFold(key, strContentLength):
			// frame-owned
		default:
			h.AddBytesKV(key, val)
		}
	})
}

func decodeResponseHeaders(h *fasthttp.ResponseHeader, raw []byte) error {
	return forEachHeader(raw, func(key, val []byte) {
		if bytesEqualFold(key, strContentLength) {
			return // frame-owned
		}
		h.AddBytesKV(key, val)
	})
}

// ---- trailers ----

// encodeTrailers writes declared trailers (rare) into the frame's trailer slot,
// in the same pair encoding as headers. None returns nil — a null slot.
func encodeTrailers(h trailerHeader) ([]byte, error) {
	keys := h.PeekTrailerKeys()
	if len(keys) == 0 {
		return nil, nil
	}
	var pairs []hpair
	for _, k := range keys {
		ck := []byte(textproto.CanonicalMIMEHeaderKey(string(k)))
		for _, v := range h.PeekAll(string(k)) {
			pairs = append(pairs, hpair{ck, v})
		}
	}
	return emitHeaders(nil, pairs), nil
}

// trailerHeader is the shared subset of *fasthttp.RequestHeader and
// *fasthttp.ResponseHeader used to read declared trailers.
type trailerHeader interface {
	PeekTrailerKeys() [][]byte
	PeekAll(key string) [][]byte
}

// applyTrailers declares each trailer name and re-adds its value(s). A name
// fasthttp forbids as a trailer (framing/routing/auth field) is carried as an
// ordinary header so no data is dropped.
func applyTrailers(h trailerSetter, raw []byte) error {
	return forEachHeader(raw, func(key, val []byte) {
		k := string(key)
		_ = h.AddTrailer(k)
		h.Add(k, string(val))
	})
}

// *fasthttp.ResponseHeader used to reapply trailers.
type trailerSetter interface {
	AddTrailer(trailer string) error
	Add(key, value string)
}
