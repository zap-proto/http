// Package zaphttp implements HTTP request/response semantics over the
// ZAP transport. Existing net/http callers and handlers work
// unmodified — the package adapts the standard library types to and
// from ZAP frames.
//
// The wire format is documented in schema/zap_http.zap (authored in
// the ZAP schema language). The bytes on the wire are
// github.com/zap-proto/go Objects — the canonical pure-stdlib ZAP
// runtime — packed at explicit byte offsets within each object's fixed
// payload. There is no external codec dependency: this file is the
// encoder and decoder.
//
// What's covered today (v0.1):
//   - Request method, target, headers, body
//   - Response status, reason, headers, body
//   - Trailers (read and write)
//
// What's not yet covered:
//   - Streaming bodies — the entire body must fit in one frame; the
//     transport negotiates a max frame size on connection setup.
//   - HTTP/2 server push semantics; HTTP/3 priorities.
//   - WebSocket / SSE upgrade paths; those land in zap-proto/ws.
//
// # Wire layout
//
// A frame is one zap-proto/go message. The message type rides in the
// header flags (flags>>8): FrameRequest for a request, FrameResponse
// for a response — the same discriminator discipline the zap forward
// envelopes use. There is no union struct; the flag selects which
// object shape is the message root.
//
// # Offset discipline
//
// zap-proto/go's ObjectBuilder.SetBytes / SetText write an 8-byte slot
// at the field offset: {relOffset uint32, length uint32}. The
// variable-length tail is appended after the fixed section in Finish()
// and the relOffset is patched to point at it. Consequently EVERY text
// or bytes field consumes 8 bytes of fixed payload — adjacent text
// fields MUST be spaced 8 bytes apart or their slots overlap and
// corrupt each other. Fixed scalars use their natural width on a
// natural boundary. These offsets are the contract; changing one is a
// wire break.
//
// Headers (and trailers) ride as a JSON map[string][]string — the
// native shape of http.Header — preserving multi-value semantics
// (RFC 9110 §5.2) losslessly without a nested-object list.
//
// This file holds the marshal/unmarshal between *http.Request /
// http.Response and ZAP frames. The transport layer (TCP framing
// today, PQ-handshake-wrapped ZAP transport in v0.2) lives in
// transport.go.

package zaphttp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	zap "github.com/zap-proto/go"
)

// Frame type IDs. The ZAP message header carries the type in the upper
// byte of the 16-bit flags field; FinishWithFlags(t<<8) tags the
// message and Message.Flags()>>8 recovers it. A request frame and a
// response frame are the two shapes the wire carries today.
const (
	FrameRequest  uint16 = 0x01
	FrameResponse uint16 = 0x02
)

// Request frame field offsets within the object's fixed payload. Every
// text/bytes slot is 8 bytes (relOffset+length); see "Offset
// discipline" in the package doc.
const (
	reqMethod   = 0  // text  (8)
	reqTarget   = 8  // text  (8)
	reqProto    = 16 // text  (8)
	reqHeaders  = 24 // bytes (8) -- JSON map[string][]string
	reqBody     = 32 // bytes (8)
	reqTrailer  = 40 // bytes (8) -- JSON map[string][]string
	reqSlotSize = 48
)

// Response frame field offsets. status@0 is a uint16 (2 bytes); the
// first text slot starts at the next 8-byte boundary.
const (
	respStatus   = 0  // uint16 (2)
	respReason   = 8  // text   (8)
	respProto    = 16 // text   (8)
	respHeaders  = 24 // bytes  (8) -- JSON map[string][]string
	respBody     = 32 // bytes  (8)
	respTrailer  = 40 // bytes  (8) -- JSON map[string][]string
	respSlotSize = 48
)

// MarshalRequest serializes an *http.Request into a ZAP frame. The
// returned bytes are framed and ready for transport.Write — the
// transport layer prepends the length prefix.
//
// The body is fully read and consumed; callers that need to reuse the
// request must replace req.Body with a fresh reader after this call.
func MarshalRequest(req *http.Request) ([]byte, error) {
	body, err := readAllBody(req.Body)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: read body: %w", err)
	}

	target := req.RequestURI
	if target == "" && req.URL != nil {
		target = req.URL.RequestURI()
	}
	headers, err := encodeHeaders(req.Header)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode headers: %w", err)
	}
	trailer, err := encodeHeaders(req.Trailer)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode trailer: %w", err)
	}

	b := zap.NewBuilder(reqSlotSize + len(body) + len(headers) + len(trailer) +
		len(req.Method) + len(target) + len(req.Proto) + 64)
	ob := b.StartObject(reqSlotSize)
	ob.SetText(reqMethod, orDefault(req.Method, http.MethodGet))
	ob.SetText(reqTarget, target)
	ob.SetText(reqProto, orDefault(req.Proto, "ZAP-HTTP/1.0"))
	ob.SetBytes(reqHeaders, headers)
	ob.SetBytes(reqBody, body)
	ob.SetBytes(reqTrailer, trailer)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameRequest << 8), nil
}

// UnmarshalRequest reconstructs an *http.Request from a ZAP frame. The
// returned request has Body set to a bytes.Reader over the frame's body
// bytes; the request URL is parsed from target. Host is taken from the
// request's Host header (RFC 9110 §7.2).
func UnmarshalRequest(b []byte) (*http.Request, error) {
	msg, err := zap.Parse(b)
	if err != nil {
		return nil, err
	}
	if t := msg.Flags() >> 8; t != FrameRequest {
		return nil, fmt.Errorf("zaphttp: frame type %#x, want request (%#x)", t, FrameRequest)
	}
	r := msg.Root()

	method := r.Text(reqMethod)
	target := r.Text(reqTarget)
	proto := r.Text(reqProto)
	headers, err := decodeHeaders(r.Bytes(reqHeaders))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: decode headers: %w", err)
	}
	body := r.Bytes(reqBody)
	trailer, err := decodeHeaders(r.Bytes(reqTrailer))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: decode trailer: %w", err)
	}

	u, err := url.ParseRequestURI(target)
	if err != nil {
		// Best-effort: keep the raw target so handlers can inspect it,
		// but failing here would break callers that only need RequestURI.
		u = &url.URL{Path: target}
	}

	out := &http.Request{
		Method:     orDefault(method, http.MethodGet),
		URL:        u,
		Proto:      orDefault(proto, "ZAP-HTTP/1.0"),
		ProtoMajor: 1,
		ProtoMinor: 0,
		Header:     headers,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Host:       headers.Get("Host"),
		RequestURI: target,
		Trailer:    trailer,
	}
	if cl := headers.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			out.ContentLength = n
		}
	} else {
		out.ContentLength = int64(len(body))
	}
	return out, nil
}

// MarshalResponse serializes an *http.Response into a ZAP frame.
func MarshalResponse(resp *http.Response) ([]byte, error) {
	body, err := readAllBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: read body: %w", err)
	}

	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	reason := resp.Status
	if i := strings.IndexByte(reason, ' '); i >= 0 {
		reason = reason[i+1:]
	}
	headers, err := encodeHeaders(resp.Header)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode headers: %w", err)
	}
	trailer, err := encodeHeaders(resp.Trailer)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: encode trailer: %w", err)
	}

	b := zap.NewBuilder(respSlotSize + len(body) + len(headers) + len(trailer) +
		len(reason) + len(resp.Proto) + 64)
	ob := b.StartObject(respSlotSize)
	ob.SetUint16(respStatus, uint16(status))
	ob.SetText(respReason, reason)
	ob.SetText(respProto, orDefault(resp.Proto, "ZAP-HTTP/1.0"))
	ob.SetBytes(respHeaders, headers)
	ob.SetBytes(respBody, body)
	ob.SetBytes(respTrailer, trailer)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameResponse << 8), nil
}

// UnmarshalResponse reconstructs an *http.Response from a ZAP frame.
func UnmarshalResponse(b []byte) (*http.Response, error) {
	msg, err := zap.Parse(b)
	if err != nil {
		return nil, err
	}
	if t := msg.Flags() >> 8; t != FrameResponse {
		return nil, fmt.Errorf("zaphttp: frame type %#x, want response (%#x)", t, FrameResponse)
	}
	r := msg.Root()

	status := int(r.Uint16(respStatus))
	reason := r.Text(respReason)
	proto := r.Text(respProto)
	headers, err := decodeHeaders(r.Bytes(respHeaders))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: decode headers: %w", err)
	}
	body := r.Bytes(respBody)
	trailer, err := decodeHeaders(r.Bytes(respTrailer))
	if err != nil {
		return nil, fmt.Errorf("zaphttp: decode trailer: %w", err)
	}

	statusText := reason
	if statusText == "" {
		statusText = http.StatusText(status)
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, statusText),
		StatusCode:    status,
		Proto:         orDefault(proto, "ZAP-HTTP/1.0"),
		ProtoMajor:    1,
		ProtoMinor:    0,
		Header:        headers,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Trailer:       trailer,
	}, nil
}

// ---- helpers ----

func readAllBody(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	defer func() {
		if c, ok := r.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	return io.ReadAll(r)
}

// encodeHeaders serializes an http.Header as a JSON map[string][]string.
// An empty header set encodes as nil (the SetBytes null slot), which
// decodeHeaders maps back to an empty, non-nil http.Header. Header is
// already map[string][]string, so this is a direct, lossless encoding
// that preserves multi-value names (RFC 9110 §5.2).
func encodeHeaders(h http.Header) ([]byte, error) {
	if len(h) == 0 {
		return nil, nil
	}
	return json.Marshal(h)
}

// decodeHeaders is the inverse of encodeHeaders. A null/empty slot
// yields an empty, non-nil http.Header so callers never see a nil map.
func decodeHeaders(b []byte) (http.Header, error) {
	if len(b) == 0 {
		return http.Header{}, nil
	}
	h := http.Header{}
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	return h, nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
