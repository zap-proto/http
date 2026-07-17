package zaphttp

import (
	"bytes"
	"encoding/json"
	"net/textproto"
	"strings"
	"testing"

	"github.com/valyala/fasthttp"
	zap "github.com/zap-proto/go"
)

// White-box tests proving the zero-alloc codec is byte-identical to the
// standard library (for the headers JSON) and to the reference zap.Builder path
// (for whole frames), plus -benchmem benchmarks documenting allocs/op.

// ---- header JSON byte-identity vs encoding/json ----

func TestAppendJSONString_matchesStdlib(t *testing.T) {
	cases := []string{
		"",
		"simple",
		"text/plain; charset=utf-8",
		"application/json",
		"Bearer abc.def.ghi",
		`he said "quote"`,
		`back\slash`,
		"tab\tnewline\ncr\r",
		"ctrl\x00\x01\x02\x1f",
		"html <b>&</b> >x<",
		"slash/and:colon;semi",
		"café ☕ 世界 — unicode",
		"line sep para", // U+2028/U+2029: json escapes, we must too
		"del\x7fbyte",
		"高性能 ZAP",
	}
	for _, s := range cases {
		got := appendJSONString(nil, []byte(s))
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("json.Marshal(%q): %v", s, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("appendJSONString(%q)\n got=%s\nwant=%s", s, got, want)
		}
	}
}

func TestEmitHeadersJSON_matchesStdlib(t *testing.T) {
	cases := []map[string][]string{
		{"Content-Type": {"application/json"}},
		{"Content-Type": {"text/plain; charset=utf-8"}},
		// multi-key: json sorts keys; we must match
		{"X-Trace-Id": {"abc-123"}, "Content-Type": {"application/json"}},
		// multi-value
		{"X-Multi": {"one", "two", "three"}},
		// keys that sort non-trivially + values needing escapes
		{"Z-Last": {"z"}, "A-First": {`a"b`}, "M-Mid": {"<m&m>"}},
		{"X-Unicode": {"café ☕"}, "X-Ascii": {"plain"}},
	}
	for _, m := range cases {
		var pairs []hpair
		for k, vals := range m {
			for _, v := range vals {
				pairs = append(pairs, hpair{[]byte(k), []byte(v)})
			}
		}
		got := emitHeadersJSON(nil, pairs)
		want, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("emitHeadersJSON(%v)\n got=%s\nwant=%s", m, got, want)
		}
	}
}

// ---- whole-frame byte-identity vs the reference zap.Builder path ----

// refMarshalRequest / refMarshalResponse reproduce the prior codec exactly
// (zap.Builder + json header map), the byte-for-byte oracle the fast codec must
// match for arbitrary inputs — not just the four captured golden frames.

func refCollect(vis interface {
	VisitAll(func(k, v []byte))
	PeekTrailerKeys() [][]byte
}) map[string][]string {
	skip := map[string]bool{}
	for _, k := range vis.PeekTrailerKeys() {
		skip[strings.ToLower(string(k))] = true
	}
	m := map[string][]string{}
	vis.VisitAll(func(key, value []byte) {
		k := string(key)
		lk := strings.ToLower(k)
		if lk == "host" || lk == "content-length" || lk == "trailer" || skip[lk] {
			return
		}
		m[k] = append(m[k], string(value))
	})
	return m
}

func refHeaderJSON(m map[string][]string) []byte {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return b
}

func refTrailerJSON(vis interface {
	PeekTrailerKeys() [][]byte
	PeekAll(string) [][]byte
}) []byte {
	keys := vis.PeekTrailerKeys()
	if len(keys) == 0 {
		return nil
	}
	m := map[string][]string{}
	for _, k := range keys {
		ck := textproto.CanonicalMIMEHeaderKey(string(k))
		for _, v := range vis.PeekAll(string(k)) {
			m[ck] = append(m[ck], string(v))
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func refMarshalRequest(req *fasthttp.Request) []byte {
	method := string(req.Header.Method())
	if method == "" {
		method = "GET"
	}
	target := string(req.RequestURI())
	proto := string(req.Header.Protocol())
	if proto == "" {
		proto = defaultProto
	}
	headers := refHeaderJSON(refCollect(&req.Header))
	trailer := refTrailerJSON(&req.Header)

	b := zap.NewBuilder(reqSlotSize + len(req.Body()) + len(headers) + len(trailer) + 64)
	ob := b.StartObject(reqSlotSize)
	ob.SetText(reqMethod, method)
	ob.SetText(reqTarget, target)
	ob.SetText(reqProto, proto)
	ob.SetBytes(reqHeaders, headers)
	ob.SetBytes(reqBody, req.Body())
	ob.SetBytes(reqTrailer, trailer)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameRequest << 8)
}

func refMarshalResponse(resp *fasthttp.Response) []byte {
	status := resp.StatusCode()
	if status == 0 {
		status = fasthttp.StatusOK
	}
	reason := string(resp.Header.StatusMessage())
	if reason == "" {
		reason = fasthttp.StatusMessage(status)
	}
	proto := string(resp.Header.Protocol())
	if proto == "" {
		proto = defaultProto
	}
	headers := refHeaderJSON(refCollect(&resp.Header))
	trailer := refTrailerJSON(&resp.Header)

	b := zap.NewBuilder(respSlotSize + len(resp.Body()) + len(headers) + len(trailer) + 64)
	ob := b.StartObject(respSlotSize)
	ob.SetUint16(respStatus, uint16(status))
	ob.SetText(respReason, reason)
	ob.SetText(respProto, proto)
	ob.SetBytes(respHeaders, headers)
	ob.SetBytes(respBody, resp.Body())
	ob.SetBytes(respTrailer, trailer)
	ob.FinishAsRoot()
	return b.FinishWithFlags(FrameResponse << 8)
}

func TestCodec_CrossCheckRequest(t *testing.T) {
	build := []func(*fasthttp.Request){
		func(r *fasthttp.Request) { r.Header.SetMethod("GET"); r.SetRequestURI("/health"); r.Header.SetProtocol("HTTP/1.1") },
		func(r *fasthttp.Request) {
			r.Header.SetMethod("POST")
			r.SetRequestURI("/v1/blocks?height=42")
			r.Header.SetProtocol("HTTP/1.1")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Trace-Id", "abc-123")
			r.SetBody([]byte(`{"jsonrpc":"2.0"}`))
		},
		func(r *fasthttp.Request) { // multi-value + special chars
			r.Header.SetMethod("PUT")
			r.SetRequestURI("/x")
			r.Header.Add("X-Multi", "a")
			r.Header.Add("X-Multi", "b")
			r.Header.Set("X-Html", "<tag>&amp;</tag>")
			r.SetBody([]byte("body-\x00\x01"))
		},
		func(r *fasthttp.Request) { // empty target edge case
			r.Header.SetMethod("OPTIONS")
			r.SetRequestURI("")
			r.Header.SetProtocol("HTTP/1.1")
		},
	}
	for i, f := range build {
		req := fasthttp.AcquireRequest()
		f(req)
		want := refMarshalRequest(req)
		got, err := MarshalRequest(req)
		if err != nil {
			t.Fatalf("case %d: MarshalRequest: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("case %d request frame mismatch\n got=%x\nwant=%x", i, got, want)
		}
		fasthttp.ReleaseRequest(req)
	}
}

func TestCodec_CrossCheckResponse(t *testing.T) {
	build := []func(*fasthttp.Response){
		func(r *fasthttp.Response) { r.SetStatusCode(200); r.Header.SetProtocol([]byte("HTTP/1.1")); r.Header.SetContentType("text/plain; charset=utf-8"); r.SetBodyString("ok") },
		func(r *fasthttp.Response) {
			r.SetStatusCode(200)
			r.Header.SetProtocol([]byte("HTTP/1.1"))
			r.Header.SetContentType("application/json")
			r.Header.Set("X-Trace-Id", "abc-123")
			r.SetBody([]byte(`{"result":"0x2a"}`))
		},
		func(r *fasthttp.Response) { // multi-value response header + status msg
			r.SetStatusCode(418)
			r.Header.SetContentType("application/octet-stream")
			r.Header.Add("X-Multi", "one")
			r.Header.Add("X-Multi", "two")
			r.SetBody([]byte("payload-\x00\x01-nuls"))
		},
		func(r *fasthttp.Response) { // trailers
			r.SetStatusCode(200)
			r.Header.SetProtocol([]byte("HTTP/1.1"))
			r.Header.SetContentType("application/octet-stream")
			_ = r.Header.AddTrailer("X-Checksum")
			r.Header.Set("X-Checksum", "deadbeef")
			r.SetBodyString("streamed")
		},
	}
	for i, f := range build {
		resp := fasthttp.AcquireResponse()
		f(resp)
		want := refMarshalResponse(resp)
		got, err := MarshalResponse(resp)
		if err != nil {
			t.Fatalf("case %d: MarshalResponse: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("case %d response frame mismatch\n got=%x\nwant=%x", i, got, want)
		}
		fasthttp.ReleaseResponse(resp)
	}
}

// ---- benchmarks: allocs/op for the codec hot path ----

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
