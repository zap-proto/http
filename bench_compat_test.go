package http_test

// Exported-API-only codec benchmarks. These use only MarshalRequest /
// MarshalResponse / UnmarshalRequest / UnmarshalResponse, so the SAME file
// compiles and runs against both v0.2.0 (the pre-optimization baseline) and the
// zero-alloc branch — an apples-to-apples before/after of allocs/op.

import (
	"testing"

	"github.com/valyala/fasthttp"
	"github.com/zap-proto/http"
)

func compatReq() *fasthttp.Request {
	req := fasthttp.AcquireRequest()
	req.Header.SetMethod("GET")
	req.SetRequestURI("/health")
	req.Header.SetHost("192.168.77.2:8391")
	return req
}

func compatResp() *fasthttp.Response {
	resp := fasthttp.AcquireResponse()
	resp.SetStatusCode(200)
	resp.Header.SetContentType("text/plain; charset=utf-8")
	resp.SetBodyString("ok")
	return resp
}

func BenchmarkCompat_MarshalRequest(b *testing.B) {
	req := compatReq()
	defer fasthttp.ReleaseRequest(req)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := http.MarshalRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompat_MarshalResponse(b *testing.B) {
	resp := compatResp()
	defer fasthttp.ReleaseResponse(resp)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := http.MarshalResponse(resp); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompat_UnmarshalRequest(b *testing.B) {
	req := compatReq()
	frame, _ := http.MarshalRequest(req)
	fasthttp.ReleaseRequest(req)
	var dst fasthttp.Request
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := http.UnmarshalRequest(frame, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompat_UnmarshalResponse(b *testing.B) {
	resp := compatResp()
	frame, _ := http.MarshalResponse(resp)
	fasthttp.ReleaseResponse(resp)
	var dst fasthttp.Response
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := http.UnmarshalResponse(frame, &dst); err != nil {
			b.Fatal(err)
		}
	}
}
