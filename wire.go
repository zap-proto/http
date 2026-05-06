// Package zaphttp implements HTTP request/response semantics over the
// ZAP transport. Existing net/http callers and handlers work
// unmodified — the package adapts the standard library types to and
// from Cap'n Proto-encoded ZAP frames.
//
// The wire format is documented in schema/zap_http.capnp; the Go types
// in internal/capnp are generated from it.
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
// This file holds the marshal/unmarshal between *http.Request /
// http.Response and ZAP frames. It depends only on the standard
// library plus Cap'n Proto. The transport layer (TCP framing today,
// PQ-handshake-wrapped ZAP transport in v0.2) lives in transport.go.

package zaphttp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"capnproto.org/go/capnp/v3"

	zhcapnp "github.com/zap-proto/http/internal/capnp"
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

	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		return nil, err
	}
	frame, err := zhcapnp.NewRootFrame(seg)
	if err != nil {
		return nil, err
	}
	r, err := frame.NewRequest()
	if err != nil {
		return nil, err
	}

	if err := r.SetMethod(orDefault(req.Method, http.MethodGet)); err != nil {
		return nil, err
	}
	target := req.RequestURI
	if target == "" && req.URL != nil {
		target = req.URL.RequestURI()
	}
	if err := r.SetTarget(target); err != nil {
		return nil, err
	}
	if err := r.SetProto(orDefault(req.Proto, "ZAP-HTTP/1.0")); err != nil {
		return nil, err
	}
	if err := writeHeaders(seg, req.Header, r.SetHeaders); err != nil {
		return nil, err
	}
	if len(body) > 0 {
		if err := r.SetBody(body); err != nil {
			return nil, err
		}
	}
	if len(req.Trailer) > 0 {
		if err := writeHeaders(seg, req.Trailer, r.SetTrailer); err != nil {
			return nil, err
		}
	}

	return msg.Marshal()
}

// UnmarshalRequest reconstructs an *http.Request from a ZAP frame. The
// returned request has Body set to a bytes.Reader over the frame's body
// bytes; the request URL is parsed from target. Host is taken from the
// request's Host header (RFC 9110 §7.2).
func UnmarshalRequest(b []byte) (*http.Request, error) {
	msg, err := capnp.Unmarshal(b)
	if err != nil {
		return nil, err
	}
	frame, err := zhcapnp.ReadRootFrame(msg)
	if err != nil {
		return nil, err
	}
	if frame.Which() != zhcapnp.Frame_Which_request {
		return nil, fmt.Errorf("zaphttp: frame is %s, want request", frame.Which())
	}
	r, err := frame.Request()
	if err != nil {
		return nil, err
	}

	method, err := r.Method()
	if err != nil {
		return nil, err
	}
	target, err := r.Target()
	if err != nil {
		return nil, err
	}
	proto, err := r.Proto()
	if err != nil {
		return nil, err
	}
	headers, err := readHeaders(r.Headers)
	if err != nil {
		return nil, err
	}
	body, err := r.Body()
	if err != nil {
		return nil, err
	}
	trailer, err := readHeaders(r.Trailer)
	if err != nil {
		return nil, err
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

	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		return nil, err
	}
	frame, err := zhcapnp.NewRootFrame(seg)
	if err != nil {
		return nil, err
	}
	r, err := frame.NewResponse()
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == 0 {
		r.SetStatus(http.StatusOK)
	} else {
		r.SetStatus(uint16(resp.StatusCode))
	}
	reason := resp.Status
	if i := strings.IndexByte(reason, ' '); i >= 0 {
		reason = reason[i+1:]
	}
	if err := r.SetReason(reason); err != nil {
		return nil, err
	}
	if err := r.SetProto(orDefault(resp.Proto, "ZAP-HTTP/1.0")); err != nil {
		return nil, err
	}
	if err := writeHeaders(seg, resp.Header, r.SetHeaders); err != nil {
		return nil, err
	}
	if len(body) > 0 {
		if err := r.SetBody(body); err != nil {
			return nil, err
		}
	}
	if len(resp.Trailer) > 0 {
		if err := writeHeaders(seg, resp.Trailer, r.SetTrailer); err != nil {
			return nil, err
		}
	}

	return msg.Marshal()
}

// UnmarshalResponse reconstructs an *http.Response from a ZAP frame.
func UnmarshalResponse(b []byte) (*http.Response, error) {
	msg, err := capnp.Unmarshal(b)
	if err != nil {
		return nil, err
	}
	frame, err := zhcapnp.ReadRootFrame(msg)
	if err != nil {
		return nil, err
	}
	if frame.Which() != zhcapnp.Frame_Which_response {
		return nil, fmt.Errorf("zaphttp: frame is %s, want response", frame.Which())
	}
	r, err := frame.Response()
	if err != nil {
		return nil, err
	}

	status := int(r.Status())
	reason, err := r.Reason()
	if err != nil {
		return nil, err
	}
	proto, err := r.Proto()
	if err != nil {
		return nil, err
	}
	headers, err := readHeaders(r.Headers)
	if err != nil {
		return nil, err
	}
	body, err := r.Body()
	if err != nil {
		return nil, err
	}
	trailer, err := readHeaders(r.Trailer)
	if err != nil {
		return nil, err
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

func writeHeaders(seg *capnp.Segment, h http.Header, setter func(zhcapnp.Header_List) error) error {
	if len(h) == 0 {
		// Allocate an empty list so consumers don't see a default-zero
		// pointer that fails to decode.
		empty, err := zhcapnp.NewHeader_List(seg, 0)
		if err != nil {
			return err
		}
		return setter(empty)
	}
	list, err := zhcapnp.NewHeader_List(seg, int32(len(h)))
	if err != nil {
		return err
	}
	i := 0
	for name, values := range h {
		hdr := list.At(i)
		if err := hdr.SetName(name); err != nil {
			return err
		}
		vlist, err := capnp.NewTextList(seg, int32(len(values)))
		if err != nil {
			return err
		}
		for j, v := range values {
			if err := vlist.Set(j, v); err != nil {
				return err
			}
		}
		if err := hdr.SetValues(vlist); err != nil {
			return err
		}
		i++
	}
	return setter(list)
}

func readHeaders(getter func() (zhcapnp.Header_List, error)) (http.Header, error) {
	list, err := getter()
	if err != nil {
		return nil, err
	}
	if list.Len() == 0 {
		return http.Header{}, nil
	}
	out := make(http.Header, list.Len())
	for i := 0; i < list.Len(); i++ {
		h := list.At(i)
		name, err := h.Name()
		if err != nil {
			return nil, err
		}
		vlist, err := h.Values()
		if err != nil {
			return nil, err
		}
		for j := 0; j < vlist.Len(); j++ {
			v, err := vlist.At(j)
			if err != nil {
				return nil, err
			}
			out.Add(name, v)
		}
	}
	return out, nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
