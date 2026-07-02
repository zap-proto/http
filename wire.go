// Package zaphttp implements HTTP request/response semantics over the
// ZAP transport for the fasthttp handler model. A fasthttp.RequestHandler
// is served over ZAP frames; a fasthttp.Request/Response pair is exchanged
// by the client. There is no net/http in the request path — Fiber and
// every other fasthttp stack hand their handler straight to this package.
//
// The wire format is documented in schema/zap_http.zap (authored in the
// ZAP schema language). The bytes on the wire are github.com/zap-proto/go
// Objects — the canonical pure-stdlib ZAP runtime — packed at explicit
// byte offsets within each object's fixed payload. There is no external
// codec dependency: this file is the encoder and decoder.
//
// # Wire compatibility
//
// The frame format is UNCHANGED from the prior net/http codec — same
// object slots, same offsets, same JSON header map, same flags
// discriminator. Only the Go type the codec reads from and writes to
// changed (net/http → fasthttp). Frames produced here are byte-identical
// to the frames the previous net/http codec produced for the same logical
// message, so a fasthttp-based peer and a net/http-based peer interoperate
// on the wire. This is verified by golden-byte tests (zaphttp_test.go)
// that decode frames captured from the previous codec and re-encode them
// to the same bytes.
//
// What's covered:
//   - Request method, target, headers, body
//   - Response status, reason, headers, body
//   - Trailers (read and write)
//
// What's not yet covered:
//   - Streaming bodies — the entire body must fit in one frame; the
//     transport negotiates a max frame size on connection setup.
//   - WebSocket / SSE upgrade paths; those land in zap-proto/ws.
//
// # Wire layout
//
// A frame is one zap-proto/go message. The message type rides in the
// header flags (flags>>8): FrameRequest for a request, FrameResponse for
// a response. There is no union struct; the flag selects which object
// shape is the message root.
//
// # Offset discipline
//
// zap-proto/go's ObjectBuilder.SetBytes / SetText write an 8-byte slot at
// the field offset: {relOffset uint32, length uint32}. The variable-length
// tail is appended after the fixed section in Finish() and the relOffset
// is patched to point at it. Consequently EVERY text or bytes field
// consumes 8 bytes of fixed payload — adjacent text fields MUST be spaced
// 8 bytes apart or their slots overlap and corrupt each other. Fixed
// scalars use their natural width on a natural boundary. These offsets are
// the contract; changing one is a wire break.
//
// # Framing metadata vs headers
//
// Three fields are owned by the frame, not the headers slot, exactly as
// the reference net/http codec treated them:
//
//   - Content-Length — the frame length-prefixes the body, so body length
//     is authoritative; it is reconstructed on decode, never carried in
//     the headers map.
//   - Host — request routing metadata; carried in fasthttp's Host slot,
//     not the headers map (matching net/http, which keeps Host out of
//     Header).
//   - Trailer (the meta-header listing trailer names) — represented by the
//     dedicated trailer slot, not the headers map.
//
// Everything else fasthttp exposes (Content-Type, User-Agent, Cookie,
// Authorization, X-*, Set-Cookie, …) rides in the headers slot as a JSON
// map[string][]string — the native lossless shape for multi-value header
// names (RFC 9110 §5.2).

package zaphttp

import (
	"encoding/json"
	"fmt"
	"net/textproto"
	"strings"

	"github.com/valyala/fasthttp"
	zap "github.com/zap-proto/go"
)

// Frame type IDs. The ZAP message header carries the type in the upper
// byte of the 16-bit flags field; FinishWithFlags(t<<8) tags the message
// and Message.Flags()>>8 recovers it. A request frame and a response frame
// are the two shapes the wire carries today.
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

// Data/End frame layout: a single bytes slot (the chunk, or the trailer JSON).
const (
	chunkBytes    = 0 // bytes (8)
	chunkSlotSize = 8
)

// FrameTypeOf peeks a frame's type from its ZAP header without decoding the
// body, so a reader can dispatch (response vs streamed head vs data vs end).
func FrameTypeOf(frame []byte) (uint16, error) {
	msg, err := zap.Parse(frame)
	if err != nil {
		return 0, err
	}
	return msg.Flags() >> 8, nil
}

// MarshalResponseHead serializes a response's status + headers (NO body) as the
// opening frame of a stream. The body is delivered by subsequent FrameData
// frames.
func MarshalResponseHead(resp *fasthttp.Response) ([]byte, error) {
	status := resp.StatusCode()
	if status == 0 {
		status = fasthttp.StatusOK
	}
	reason := string(resp.Header.StatusMessage())
	if reason == "" {
		reason = fasthttp.StatusMessage(status)
	}
	proto := string(resp.Header.Protocol())
	headers, err := encodeHeaderMap(collectResponseHeaders(&resp.Header))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode headers: %w", err)
	}

	b := zap.NewBuilder(respSlotSize + len(headers) + len(reason) + len(proto) + 64)
	ob := b.StartObject(respSlotSize)
	ob.SetUint16(respStatus, uint16(status))
	ob.SetText(respReason, reason)
	ob.SetText(respProto, orDefault(proto, defaultProto))
	ob.SetBytes(respHeaders, headers)
	ob.SetBytes(respBody, nil)
	ob.SetBytes(respTrailer, nil)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameResponseHead << 8), nil
}

// UnmarshalResponseHead applies a streamed response's status + headers to dst.
// The body is left empty; the caller attaches a streaming body that reads the
// following FrameData frames.
func UnmarshalResponseHead(frame []byte, dst *fasthttp.Response) error {
	msg, err := zap.Parse(frame)
	if err != nil {
		return err
	}
	if t := msg.Flags() >> 8; t != FrameResponseHead {
		return fmt.Errorf("zaphttp: frame type %#x, want response-head (%#x)", t, FrameResponseHead)
	}
	r := msg.Root()
	dst.Reset()
	dst.SetStatusCode(int(r.Uint16(respStatus)))
	if reason := r.Text(respReason); reason != "" {
		dst.Header.SetStatusMessage([]byte(reason))
	}
	if proto := r.Text(respProto); proto != "" {
		dst.Header.SetProtocol([]byte(proto))
	}
	return applyHeadersToResponse(&dst.Header, r.Bytes(respHeaders))
}

// MarshalData wraps one body chunk as a FrameData frame.
func MarshalData(chunk []byte) []byte {
	b := zap.NewBuilder(chunkSlotSize + len(chunk) + 32)
	ob := b.StartObject(chunkSlotSize)
	ob.SetBytes(chunkBytes, chunk)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameData << 8)
}

// DataChunkOf extracts the chunk bytes from a FrameData frame. The returned
// slice aliases the frame buffer; copy it to retain past the frame's lifetime.
func DataChunkOf(frame []byte) ([]byte, error) {
	msg, err := zap.Parse(frame)
	if err != nil {
		return nil, err
	}
	if t := msg.Flags() >> 8; t != FrameData {
		return nil, fmt.Errorf("zaphttp: frame type %#x, want data (%#x)", t, FrameData)
	}
	return msg.Root().Bytes(chunkBytes), nil
}

// MarshalEnd terminates a stream, carrying optional encoded trailers.
func MarshalEnd(trailer []byte) []byte {
	b := zap.NewBuilder(chunkSlotSize + len(trailer) + 32)
	ob := b.StartObject(chunkSlotSize)
	ob.SetBytes(chunkBytes, trailer)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameEnd << 8)
}

// Request frame field offsets within the object's fixed payload. Every
// text/bytes slot is 8 bytes (relOffset+length); see "Offset discipline"
// in the package doc.
const (
	reqMethod   = 0  // text  (8)
	reqTarget   = 8  // text  (8)
	reqProto    = 16 // text  (8)
	reqHeaders  = 24 // bytes (8) -- JSON map[string][]string
	reqBody     = 32 // bytes (8)
	reqTrailer  = 40 // bytes (8) -- JSON map[string][]string
	reqSlotSize = 48
)

// Response frame field offsets. status@0 is a uint16 (2 bytes); the first
// text slot starts at the next 8-byte boundary.
const (
	respStatus   = 0  // uint16 (2)
	respReason   = 8  // text   (8)
	respProto    = 16 // text   (8)
	respHeaders  = 24 // bytes  (8) -- JSON map[string][]string
	respBody     = 32 // bytes  (8)
	respTrailer  = 40 // bytes  (8) -- JSON map[string][]string
	respSlotSize = 48
)

// defaultProto is the proto string written when the source carries none.
// fasthttp always reports a protocol ("HTTP/1.1" by default), so this
// only guards a hand-built zero-value header.
const defaultProto = "ZAP-HTTP/1.0"

// MarshalRequest serializes a *fasthttp.Request into a ZAP frame. The
// returned bytes are the ZAP message; the transport layer prepends the
// length prefix (see transport.go).
func MarshalRequest(req *fasthttp.Request) ([]byte, error) {
	method := string(req.Header.Method())
	target := string(req.RequestURI())
	proto := string(req.Header.Protocol())
	body := req.Body()

	headers, err := encodeHeaderMap(collectRequestHeaders(&req.Header))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode headers: %w", err)
	}
	trailer, err := encodeHeaderMap(collectTrailers(&req.Header))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode trailer: %w", err)
	}

	b := zap.NewBuilder(reqSlotSize + len(body) + len(headers) + len(trailer) +
		len(method) + len(target) + len(proto) + 64)
	ob := b.StartObject(reqSlotSize)
	ob.SetText(reqMethod, orDefault(method, fasthttp.MethodGet))
	ob.SetText(reqTarget, target)
	ob.SetText(reqProto, orDefault(proto, defaultProto))
	ob.SetBytes(reqHeaders, headers)
	ob.SetBytes(reqBody, body)
	ob.SetBytes(reqTrailer, trailer)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameRequest << 8), nil
}

// UnmarshalRequest reconstructs a request into dst from a ZAP frame. dst is
// reset first, then populated: method, request-URI, protocol, headers,
// trailers, and body. Content-Length is derived from the body length by
// SetBody; Host is taken from a Host header if the frame carried one.
func UnmarshalRequest(frame []byte, dst *fasthttp.Request) error {
	msg, err := zap.Parse(frame)
	if err != nil {
		return err
	}
	if t := msg.Flags() >> 8; t != FrameRequest {
		return fmt.Errorf("zaphttp: frame type %#x, want request (%#x)", t, FrameRequest)
	}
	r := msg.Root()

	method := r.Text(reqMethod)
	target := r.Text(reqTarget)
	proto := r.Text(reqProto)
	headers := r.Bytes(reqHeaders)
	body := r.Bytes(reqBody)
	trailer := r.Bytes(reqTrailer)

	dst.Reset()
	dst.Header.SetMethod(orDefault(method, fasthttp.MethodGet))
	dst.SetRequestURI(target)
	if proto != "" {
		dst.Header.SetProtocol(proto)
	}
	if err := applyHeadersToRequest(&dst.Header, headers); err != nil {
		return fmt.Errorf("zaphttp: decode headers: %w", err)
	}
	if err := applyTrailers(&dst.Header, trailer); err != nil {
		return fmt.Errorf("zaphttp: decode trailer: %w", err)
	}
	dst.SetBody(body)
	return nil
}

// MarshalResponse serializes a *fasthttp.Response into a ZAP frame.
func MarshalResponse(resp *fasthttp.Response) ([]byte, error) {
	status := resp.StatusCode()
	if status == 0 {
		status = fasthttp.StatusOK
	}
	reason := string(resp.Header.StatusMessage())
	if reason == "" {
		reason = fasthttp.StatusMessage(status)
	}
	proto := string(resp.Header.Protocol())
	body := resp.Body()

	headers, err := encodeHeaderMap(collectResponseHeaders(&resp.Header))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode headers: %w", err)
	}
	trailer, err := encodeHeaderMap(collectTrailers(&resp.Header))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode trailer: %w", err)
	}

	b := zap.NewBuilder(respSlotSize + len(body) + len(headers) + len(trailer) +
		len(reason) + len(proto) + 64)
	ob := b.StartObject(respSlotSize)
	ob.SetUint16(respStatus, uint16(status))
	ob.SetText(respReason, reason)
	ob.SetText(respProto, orDefault(proto, defaultProto))
	ob.SetBytes(respHeaders, headers)
	ob.SetBytes(respBody, body)
	ob.SetBytes(respTrailer, trailer)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameResponse << 8), nil
}

// UnmarshalResponse reconstructs a response into dst from a ZAP frame.
func UnmarshalResponse(frame []byte, dst *fasthttp.Response) error {
	msg, err := zap.Parse(frame)
	if err != nil {
		return err
	}
	if t := msg.Flags() >> 8; t != FrameResponse {
		return fmt.Errorf("zaphttp: frame type %#x, want response (%#x)", t, FrameResponse)
	}
	r := msg.Root()

	status := int(r.Uint16(respStatus))
	reason := r.Text(respReason)
	proto := r.Text(respProto)
	headers := r.Bytes(respHeaders)
	body := r.Bytes(respBody)
	trailer := r.Bytes(respTrailer)

	dst.Reset()
	dst.SetStatusCode(status)
	if reason != "" {
		dst.Header.SetStatusMessage([]byte(reason))
	}
	if proto != "" {
		dst.Header.SetProtocol([]byte(proto))
	}
	if err := applyHeadersToResponse(&dst.Header, headers); err != nil {
		return fmt.Errorf("zaphttp: decode headers: %w", err)
	}
	if err := applyTrailers(&dst.Header, trailer); err != nil {
		return fmt.Errorf("zaphttp: decode trailer: %w", err)
	}
	dst.SetBody(body)
	return nil
}

// ---- header extraction (fasthttp -> map) ----

// collectRequestHeaders walks a request's headers into a map[string][]string,
// dropping the frame-owned fields (Host, Content-Length, the Trailer
// meta-header, and any declared-trailer values which ride in the trailer
// slot instead).
func collectRequestHeaders(h *fasthttp.RequestHeader) map[string][]string {
	skip := trailerKeySet(h)
	m := map[string][]string{}
	h.VisitAll(func(key, value []byte) {
		k := string(key)
		if isFrameOwnedHeader(k) || skip[textproto.CanonicalMIMEHeaderKey(k)] {
			return
		}
		m[k] = append(m[k], string(value))
	})
	return m
}

// collectResponseHeaders is the response counterpart of
// collectRequestHeaders. Responses have no Host, but the same
// Content-Length / Trailer-meta / declared-trailer exclusions apply.
func collectResponseHeaders(h *fasthttp.ResponseHeader) map[string][]string {
	skip := trailerKeySet(h)
	m := map[string][]string{}
	h.VisitAll(func(key, value []byte) {
		k := string(key)
		if isFrameOwnedHeader(k) || skip[textproto.CanonicalMIMEHeaderKey(k)] {
			return
		}
		m[k] = append(m[k], string(value))
	})
	return m
}

// collectTrailers reads declared trailer names and their values (stored in
// the general header store) into a map[string][]string. Works for both
// request and response headers via the trailerHeader interface.
func collectTrailers(h trailerHeader) map[string][]string {
	keys := h.PeekTrailerKeys()
	if len(keys) == 0 {
		return nil
	}
	m := map[string][]string{}
	for _, k := range keys {
		ck := textproto.CanonicalMIMEHeaderKey(string(k))
		for _, v := range h.PeekAll(string(k)) {
			m[ck] = append(m[ck], string(v))
		}
	}
	return m
}

// trailerHeader is the shared subset of *fasthttp.RequestHeader and
// *fasthttp.ResponseHeader used to read declared trailers.
type trailerHeader interface {
	PeekTrailerKeys() [][]byte
	PeekAll(key string) [][]byte
}

// trailerKeySet returns the canonicalized set of declared trailer names so
// their values can be excluded from the headers slot (they belong in the
// trailer slot).
func trailerKeySet(h trailerHeader) map[string]bool {
	keys := h.PeekTrailerKeys()
	if len(keys) == 0 {
		return nil
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[textproto.CanonicalMIMEHeaderKey(string(k))] = true
	}
	return set
}

// isFrameOwnedHeader reports whether a header name is owned by the ZAP
// frame rather than the headers map: Host and Content-Length are
// reconstructed at the boundary, and "Trailer" is represented by the
// trailer slot.
func isFrameOwnedHeader(key string) bool {
	return strings.EqualFold(key, fasthttp.HeaderHost) ||
		strings.EqualFold(key, fasthttp.HeaderContentLength) ||
		strings.EqualFold(key, fasthttp.HeaderTrailer)
}

// encodeHeaderMap serializes a header map as JSON. An empty map encodes as
// nil (the SetBytes null slot), which the decode side maps back to no
// headers — byte-identical to the reference codec's empty-header path.
func encodeHeaderMap(m map[string][]string) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return json.Marshal(m)
}

// ---- header application (map -> fasthttp) ----

func decodeHeaderMap(b []byte) (map[string][]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	m := map[string][]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func applyHeadersToRequest(h *fasthttp.RequestHeader, raw []byte) error {
	m, err := decodeHeaderMap(raw)
	if err != nil {
		return err
	}
	for k, vals := range m {
		switch {
		case strings.EqualFold(k, fasthttp.HeaderHost):
			if len(vals) > 0 {
				h.SetHost(vals[len(vals)-1])
			}
		case strings.EqualFold(k, fasthttp.HeaderContentLength):
			// frame-owned; SetBody sets the authoritative length.
		default:
			for _, v := range vals {
				h.Add(k, v)
			}
		}
	}
	return nil
}

func applyHeadersToResponse(h *fasthttp.ResponseHeader, raw []byte) error {
	m, err := decodeHeaderMap(raw)
	if err != nil {
		return err
	}
	for k, vals := range m {
		if strings.EqualFold(k, fasthttp.HeaderContentLength) {
			continue // frame-owned
		}
		for _, v := range vals {
			h.Add(k, v)
		}
	}
	return nil
}

// trailerSetter is the shared subset of *fasthttp.RequestHeader and
// *fasthttp.ResponseHeader used to reapply trailers.
type trailerSetter interface {
	AddTrailer(trailer string) error
	Add(key, value string)
}

// applyTrailers declares each trailer name and re-adds its value(s). A name
// fasthttp forbids as a trailer (framing/routing/auth field) is carried as
// an ordinary header so no data is dropped — lenient by design, since the
// frame's trailer slot is opaque JSON.
func applyTrailers(h trailerSetter, raw []byte) error {
	m, err := decodeHeaderMap(raw)
	if err != nil {
		return err
	}
	for k, vals := range m {
		_ = h.AddTrailer(k)
		for _, v := range vals {
			h.Add(k, v)
		}
	}
	return nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
