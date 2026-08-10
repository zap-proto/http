package http

import (
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
)

// White-box tests for the header pair encoding and -benchmem benchmarks
// documenting allocs/op.
//
// There used to be a second, JSON-shaped encoder here kept byte-identical to
// encoding/json, plus a reference implementation to cross-check the fast one
// against. Both are gone: headers are length-prefixed pairs now, there is one
// encoder and one decoder, and nothing to keep in sync.

// TestHeaders_RoundTrip covers exactly the values JSON needed escaping for —
// quotes, backslashes, HTML bytes, non-ASCII — plus a repeated name, which the
// pair encoding carries without needing to model a list.
func TestHeaders_RoundTrip(t *testing.T) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI("http://example.test/v1/x")
	req.Header.SetMethod("POST")
	req.Header.Add("X-Multi", "one")
	req.Header.Add("X-Multi", "two")
	req.Header.Set("X-Quote", `he said "hi" \ <b>&</b>`)
	req.Header.Set("X-Unicode", "héllo → 世界")
	req.SetBody([]byte("body"))

	frame, err := MarshalRequest(req)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	var got fasthttp.Request
	if err := UnmarshalRequest(frame, &got); err != nil {
		t.Fatalf("UnmarshalRequest: %v", err)
	}

	if v := string(got.Header.Peek("X-Quote")); v != `he said "hi" \ <b>&</b>` {
		t.Errorf("X-Quote = %q", v)
	}
	if v := string(got.Header.Peek("X-Unicode")); v != "héllo → 世界" {
		t.Errorf("X-Unicode = %q", v)
	}
	var multi []string
	for _, v := range got.Header.PeekAll("X-Multi") {
		multi = append(multi, string(v))
	}
	if strings.Join(multi, ",") != "one,two" {
		t.Errorf("X-Multi = %v, want [one two] in order", multi)
	}
	if string(got.Body()) != "body" {
		t.Errorf("body = %q", got.Body())
	}
}

// TestHeaders_Empty proves a message with no headers encodes a null slot and
// decodes back to nothing, rather than erroring on a zero-length block.
func TestHeaders_Empty(t *testing.T) {
	var n int
	if err := forEachHeader(nil, func(k, v []byte) { n++ }); err != nil {
		t.Fatalf("empty block: %v", err)
	}
	if n != 0 {
		t.Fatalf("empty block yielded %d pairs", n)
	}
}

// TestHeaders_Truncated proves a malformed block is rejected rather than read
// past its end — the decoder indexes into the frame, so a bad length must not
// become an out-of-range read.
func TestHeaders_Truncated(t *testing.T) {
	for name, raw := range map[string][]byte{
		"short count":     {1, 0, 0},
		"name len past end": {1, 0, 0, 0, 9, 0, 0, 0, 'a', 'b'},
		"missing value":   {1, 0, 0, 0, 1, 0, 0, 0, 'a'},
		"value past end":  {1, 0, 0, 0, 1, 0, 0, 0, 'a', 9, 0, 0, 0, 'b'},
	} {
		if err := forEachHeader(raw, func(k, v []byte) {}); err == nil {
			t.Errorf("%s: got nil error, want a truncation error", name)
		}
	}
}

// TestHeaders_DecodeIsZeroAlloc pins the zero-copy claim: names and values are
// subslices of the frame, so walking a header block allocates nothing.
func TestHeaders_DecodeIsZeroAlloc(t *testing.T) {
	raw := emitHeaders(nil, []hpair{
		{[]byte("Content-Type"), []byte("application/json")},
		{[]byte("X-Request-Id"), []byte("abc123")},
		{[]byte("X-Multi"), []byte("one")},
		{[]byte("X-Multi"), []byte("two")},
	})
	var sink int
	got := testing.AllocsPerRun(200, func() {
		_ = forEachHeader(raw, func(k, v []byte) { sink += len(k) + len(v) })
	})
	if got != 0 {
		t.Fatalf("forEachHeader allocated %.0f times per run, want 0", got)
	}
	_ = sink
}

func benchRequest() *fasthttp.Request {
	req := fasthttp.AcquireRequest()
	req.Header.SetMethod("GET")
	req.SetRequestURI("/health")
	req.Header.SetHost("192.168.77.2:8391")
	return req
}

func benchResponse() *fasthttp.Response {
	resp := fasthttp.AcquireResponse()
	resp.SetStatusCode(200)
	resp.Header.SetContentType("text/plain; charset=utf-8")
	resp.SetBodyString("ok")
	return resp
}

// BenchmarkMarshalRequest is the dst==nil path (allocates the result slice).
func BenchmarkMarshalRequest(b *testing.B) {
	req := benchRequest()
	defer fasthttp.ReleaseRequest(req)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := MarshalRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendRequest is the reused-buffer path the server/client use — the
// steady-state hot path. Should be zero-alloc.
func BenchmarkAppendRequest(b *testing.B) {
	req := benchRequest()
	defer fasthttp.ReleaseRequest(req)
	buf := make([]byte, 0, 512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		buf, err = AppendRequest(buf[:0], req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalResponse(b *testing.B) {
	resp := benchResponse()
	defer fasthttp.ReleaseResponse(resp)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := MarshalResponse(resp); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendResponse(b *testing.B) {
	resp := benchResponse()
	defer fasthttp.ReleaseResponse(resp)
	buf := make([]byte, 0, 512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		buf, err = AppendResponse(buf[:0], resp)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalRequest(b *testing.B) {
	req := benchRequest()
	frame, _ := MarshalRequest(req)
	fasthttp.ReleaseRequest(req)
	var dst fasthttp.Request
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := UnmarshalRequest(frame, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalResponse(b *testing.B) {
	resp := benchResponse()
	frame, _ := MarshalResponse(resp)
	fasthttp.ReleaseResponse(resp)
	var dst fasthttp.Response
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := UnmarshalResponse(frame, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

// The benchmarks above deliberately use a minimal message, and that is a trap
// worth naming: benchRequest sets only Host, which isFrameOwnedHeaderBytes
// strips, so its headers slot is NULL and forEachHeader returns on its first
// line. Those numbers therefore say nothing about the header codec — they
// measure the frame envelope. The fixtures below carry a realistic header set
// so the codec being changed is the codec being measured.

func realRequest() *fasthttp.Request {
	req := fasthttp.AcquireRequest()
	req.Header.SetMethod("POST")
	req.SetRequestURI("/v1/chat/completions")
	req.Header.SetHost("api.hanzo.ai")
	req.Header.SetContentType("application/json")
	req.Header.Set("Authorization", "Bearer sk-proj-0123456789abcdef0123456789abcdef")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("User-Agent", "hanzo-sdk-go/1.4.2 (linux; arm64)")
	req.Header.Set("X-Request-Id", "01J8Z9QK4E7M2N5P8R1T3V6W9Y")
	req.Header.Set("X-Org-Id", "hanzo")
	req.Header.Set("X-User-Id", "z")
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.SetBodyString(`{"model":"zen-1","messages":[{"role":"user","content":"hi"}]}`)
	return req
}

func realResponse() *fasthttp.Response {
	resp := fasthttp.AcquireResponse()
	resp.SetStatusCode(200)
	resp.Header.SetContentType("application/json")
	resp.Header.Set("X-Request-Id", "01J8Z9QK4E7M2N5P8R1T3V6W9Y")
	resp.Header.Set("X-Ratelimit-Limit", "10000")
	resp.Header.Set("X-Ratelimit-Remaining", "9987")
	resp.Header.Set("Cache-Control", "no-store")
	resp.Header.Set("Vary", "Accept-Encoding, Authorization")
	// A perfectly ordinary redirect target. Under the old JSON codec the '&'
	// alone forced the WHOLE message onto the allocating fallback, because
	// encoding/json escapes it and the fast path bailed on any escape.
	resp.Header.Set("Location", "https://ex.test/cb?code=abc&state=xyz")
	resp.SetBodyString(`{"id":"chatcmpl-9","object":"chat.completion","choices":[]}`)
	return resp
}

func BenchmarkAppendRequestReal(b *testing.B) {
	req := realRequest()
	defer fasthttp.ReleaseRequest(req)
	buf := make([]byte, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, _ = AppendRequest(buf[:0], req)
	}
}

func BenchmarkAppendResponseReal(b *testing.B) {
	resp := realResponse()
	defer fasthttp.ReleaseResponse(resp)
	buf := make([]byte, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, _ = AppendResponse(buf[:0], resp)
	}
}

func BenchmarkUnmarshalRequestReal(b *testing.B) {
	req := realRequest()
	defer fasthttp.ReleaseRequest(req)
	frame, _ := MarshalRequest(req)
	var dst fasthttp.Request
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = UnmarshalRequest(frame, &dst)
	}
}

func BenchmarkUnmarshalResponseReal(b *testing.B) {
	resp := realResponse()
	defer fasthttp.ReleaseResponse(resp)
	frame, _ := MarshalResponse(resp)
	var dst fasthttp.Response
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = UnmarshalResponse(frame, &dst)
	}
}
