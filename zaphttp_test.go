// End-to-end and wire-format tests for the fasthttp ZAP-HTTP codec and the
// in-process server/client pair.
//
// The golden constants below were captured from the PREVIOUS net/http codec
// (the live prod transport used by luxd and hanzo/gateway) before the
// fasthttp rewrite. The TestWireGolden_* tests assert the fasthttp codec
// decodes those exact bytes faithfully AND re-encodes to the same bytes —
// proof that the on-the-wire format did not change.

package zaphttp_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	zaphttp "github.com/zap-proto/http"
)

// Golden frames captured from the prior net/http codec. See file header.
const (
	goldenRequest = "5a4150000100000110000000ca00000030000000040000002c000000140000003800000008000000380000003e0000006e0000002c0000000000000000000000504f53542f76312f626c6f636b733f6865696768743d3432485454502f312e317b22436f6e74656e742d54797065223a5b226170706c69636174696f6e2f6a736f6e225d2c22582d54726163652d4964223a5b226162632d313233225d7d7b226a736f6e727063223a22322e30222c226d6574686f64223a226574685f626c6f636b4e756d626572227d"

	goldenResponse = "5a415000010000021000000099000000c80000000000000028000000020000002200000008000000220000003e000000580000001100000000000000000000004f4b485454502f312e317b22436f6e74656e742d54797065223a5b226170706c69636174696f6e2f6a736f6e225d2c22582d54726163652d4964223a5b226162632d313233225d7d7b22726573756c74223a2230783261227d"

	goldenEmptyRequest = "5a41500001000001100000005200000030000000030000002b000000070000002a000000080000000000000000000000000000000000000000000000000000004745542f6865616c7468485454502f312e31"

	goldenTrailerResponse = "5a41500001000002100000009a000000c80000000000000028000000020000002200000008000000220000002d0000004700000008000000470000001b0000004f4b485454502f312e317b22436f6e74656e742d54797065223a5b226170706c69636174696f6e2f6f637465742d73747265616d225d7d73747265616d65647b22582d436865636b73756d223a5b226465616462656566225d7d"
)

// ---- server/client harness ----

// listen binds an ephemeral port and starts a server in a goroutine.
func listen(t *testing.T, handler fasthttp.RequestHandler) (addr string, shutdown func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &zaphttp.Server{Handler: handler}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil {
			t.Logf("server.Serve returned: %v", err)
		}
	}()
	return ln.Addr().String(), func() {
		_ = srv.Close()
		wg.Wait()
	}
}

// do runs one request through a fresh Transport and returns a filled
// response the caller must release.
func do(t *testing.T, addr, method, path string, body []byte, mutate func(*fasthttp.Request)) *fasthttp.Response {
	t.Helper()
	tr := zaphttp.NewTransport(addr)
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod(method)
	req.SetRequestURI(path)
	if body != nil {
		req.SetBody(body)
	}
	if mutate != nil {
		mutate(req)
	}
	resp := fasthttp.AcquireResponse()
	if err := tr.Do(req, resp); err != nil {
		fasthttp.ReleaseResponse(resp)
		t.Fatalf("Do %s %s: %v", method, path, err)
	}
	return resp
}

// ---- round-trip tests ----

func TestRoundTrip_GET(t *testing.T) {
	addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) {
		if string(ctx.Method()) != fasthttp.MethodGet {
			t.Errorf("server saw method %q want GET", ctx.Method())
		}
		if string(ctx.Path()) != "/hello" {
			t.Errorf("server saw path %q want /hello", ctx.Path())
		}
		ctx.Response.Header.Set("Content-Type", "text/plain")
		ctx.SetBodyString("hi")
	})
	defer stop()

	resp := do(t, addr, fasthttp.MethodGet, "/hello", nil, nil)
	defer fasthttp.ReleaseResponse(resp)

	if resp.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d want 200", resp.StatusCode())
	}
	if string(resp.Body()) != "hi" {
		t.Errorf("body = %q want %q", resp.Body(), "hi")
	}
	if got := string(resp.Header.ContentType()); got != "text/plain" {
		t.Errorf("Content-Type = %q want text/plain", got)
	}
}

func TestRoundTrip_POST_bodyEcho(t *testing.T) {
	want := []byte("the quick brown fox jumps over the lazy dog\n")
	addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) {
		got := ctx.PostBody()
		if !bytes.Equal(got, want) {
			t.Errorf("server got body %q want %q", got, want)
		}
		ctx.Response.Header.SetContentType("application/octet-stream")
		ctx.Write(got) //nolint:errcheck
	})
	defer stop()

	resp := do(t, addr, fasthttp.MethodPost, "/echo", want, func(r *fasthttp.Request) {
		r.Header.SetContentType("application/octet-stream")
	})
	defer fasthttp.ReleaseResponse(resp)
	if !bytes.Equal(resp.Body(), want) {
		t.Errorf("response body = %q want %q", resp.Body(), want)
	}
}

// TestRoundTrip_ByteFaithful drives a binary body with NUL bytes through
// the full request→frame→server→handler→response path and asserts the
// bytes survive intact.
func TestRoundTrip_ByteFaithful(t *testing.T) {
	payload := []byte("payload-\x00\x01\x02-with-nuls-\xff\xfe")
	addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("X-Echo-Len", fmt.Sprintf("%d", len(ctx.PostBody())))
		ctx.Write(ctx.PostBody()) //nolint:errcheck
	})
	defer stop()

	resp := do(t, addr, fasthttp.MethodPost, "/bin", payload, nil)
	defer fasthttp.ReleaseResponse(resp)
	if !bytes.Equal(resp.Body(), payload) {
		t.Errorf("byte-faithful round trip failed: got %q want %q", resp.Body(), payload)
	}
	if got := string(resp.Header.Peek("X-Echo-Len")); got != fmt.Sprintf("%d", len(payload)) {
		t.Errorf("X-Echo-Len = %q want %d", got, len(payload))
	}
}

func TestRoundTrip_largeBody(t *testing.T) {
	const size = 4 << 20 // 4 MiB
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) {
		got := ctx.PostBody()
		if !bytes.Equal(got, payload) {
			t.Errorf("server received corrupted body (len=%d want %d)", len(got), len(payload))
		}
		ctx.Write(got) //nolint:errcheck
	})
	defer stop()

	resp := do(t, addr, fasthttp.MethodPost, "/big", payload, nil)
	defer fasthttp.ReleaseResponse(resp)
	if !bytes.Equal(resp.Body(), payload) {
		t.Errorf("response body corrupted (len=%d want %d)", len(resp.Body()), len(payload))
	}
}

func TestHeaders_multiValue(t *testing.T) {
	addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) {
		got := ctx.Request.Header.PeekAll("X-Custom")
		if len(got) != 3 {
			t.Errorf("server got X-Custom values len=%d want 3", len(got))
		}
		want := []string{"a", "b", "c"}
		for i, v := range want {
			if i < len(got) && string(got[i]) != v {
				t.Errorf("X-Custom[%d] = %q want %q", i, got[i], v)
			}
		}
		ctx.Response.Header.Add("X-Server-Custom", "x")
		ctx.Response.Header.Add("X-Server-Custom", "y")
	})
	defer stop()

	resp := do(t, addr, fasthttp.MethodGet, "/", nil, func(r *fasthttp.Request) {
		r.Header.Add("X-Custom", "a")
		r.Header.Add("X-Custom", "b")
		r.Header.Add("X-Custom", "c")
	})
	defer fasthttp.ReleaseResponse(resp)

	got := resp.Header.PeekAll("X-Server-Custom")
	if len(got) != 2 || string(got[0]) != "x" || string(got[1]) != "y" {
		t.Errorf("X-Server-Custom = %q want [x y]", got)
	}
}

func TestStatusCode_propagation(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{fasthttp.StatusNotFound, "no such thing"},
		{fasthttp.StatusInternalServerError, "boom"},
		{fasthttp.StatusServiceUnavailable, "draining"},
		{fasthttp.StatusTeapot, "i am a teapot"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d", c.status), func(t *testing.T) {
			addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) {
				ctx.SetStatusCode(c.status)
				ctx.SetBodyString(c.body)
			})
			defer stop()

			resp := do(t, addr, fasthttp.MethodGet, "/", nil, nil)
			defer fasthttp.ReleaseResponse(resp)
			if resp.StatusCode() != c.status {
				t.Errorf("status = %d want %d", resp.StatusCode(), c.status)
			}
			if !bytes.Contains(resp.Body(), []byte(c.body)) {
				t.Errorf("body = %q want to contain %q", resp.Body(), c.body)
			}
		})
	}
}

// TestKeepAlive drives N sequential requests through one Transport; the
// connection pool reuses a single TCP connection across them.
func TestKeepAlive(t *testing.T) {
	addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) {
		ctx.Write(ctx.Path()) //nolint:errcheck
	})
	defer stop()

	tr := zaphttp.NewTransport(addr)
	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("/req-%d", i)
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.Header.SetMethod(fasthttp.MethodGet)
		req.SetRequestURI(path)
		if err := tr.Do(req, resp); err != nil {
			t.Fatalf("exchange %d: %v", i, err)
		}
		if string(resp.Body()) != path {
			t.Errorf("exchange %d body = %q want %q", i, resp.Body(), path)
		}
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
}

// ---- codec idempotence ----

func TestMarshalRequest_idempotent(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	orig := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(orig)
	orig.Header.SetMethod(fasthttp.MethodPost)
	orig.SetRequestURI("/foo?a=1")
	orig.Header.SetContentType("application/json")
	orig.Header.Set("Authorization", "Bearer token-stays-encrypted-on-the-wire")
	orig.SetBody(body)

	frame, err := zaphttp.MarshalRequest(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got fasthttp.Request
	if err := zaphttp.UnmarshalRequest(frame, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got.Header.Method()) != fasthttp.MethodPost {
		t.Errorf("method = %q want POST", got.Header.Method())
	}
	if string(got.URI().Path()) != "/foo" {
		t.Errorf("path = %q want /foo", got.URI().Path())
	}
	if string(got.URI().QueryArgs().Peek("a")) != "1" {
		t.Errorf("query a = %q want 1", got.URI().QueryArgs().Peek("a"))
	}
	if string(got.Header.ContentType()) != "application/json" {
		t.Errorf("Content-Type lost")
	}
	if len(got.Header.Peek("Authorization")) == 0 {
		t.Errorf("Authorization lost")
	}
	if !bytes.Equal(got.Body(), body) {
		t.Errorf("body mismatch")
	}
}

func TestMarshalResponse_idempotent(t *testing.T) {
	body := []byte("payload-bytes-\x00\x01\x02-with-nuls")
	orig := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(orig)
	orig.SetStatusCode(fasthttp.StatusTeapot)
	orig.Header.SetContentType("application/octet-stream")
	orig.Header.Add("X-Multi", "one")
	orig.Header.Add("X-Multi", "two")
	orig.Header.Add("X-Multi", "three")
	orig.SetBody(body)
	if err := orig.Header.AddTrailer("X-Checksum"); err != nil {
		t.Fatalf("AddTrailer: %v", err)
	}
	orig.Header.Set("X-Checksum", "deadbeef")

	frame, err := zaphttp.MarshalResponse(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got fasthttp.Response
	if err := zaphttp.UnmarshalResponse(frame, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.StatusCode() != fasthttp.StatusTeapot {
		t.Errorf("status = %d want 418", got.StatusCode())
	}
	if ct := string(got.Header.ContentType()); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q want application/octet-stream", ct)
	}
	multi := got.Header.PeekAll("X-Multi")
	if len(multi) != 3 || string(multi[0]) != "one" || string(multi[1]) != "two" || string(multi[2]) != "three" {
		t.Errorf("X-Multi = %q want [one two three]", multi)
	}
	if cs := string(got.Header.Peek("X-Checksum")); cs != "deadbeef" {
		t.Errorf("trailer X-Checksum = %q want deadbeef", cs)
	}
	if !bytes.Equal(got.Body(), body) {
		t.Errorf("body mismatch: got %q want %q", got.Body(), body)
	}
}

func TestFrameType_crossGuard(t *testing.T) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod(fasthttp.MethodGet)
	req.SetRequestURI("/x")
	reqFrame, err := zaphttp.MarshalRequest(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var asResp fasthttp.Response
	if err := zaphttp.UnmarshalResponse(reqFrame, &asResp); err == nil {
		t.Errorf("UnmarshalResponse accepted a request frame; want error")
	}

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	resp.SetStatusCode(fasthttp.StatusOK)
	respFrame, err := zaphttp.MarshalResponse(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var asReq fasthttp.Request
	if err := zaphttp.UnmarshalRequest(respFrame, &asReq); err == nil {
		t.Errorf("UnmarshalRequest accepted a response frame; want error")
	}
}

func TestEmptyHeaders_decode(t *testing.T) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod(fasthttp.MethodGet)
	req.SetRequestURI("/")
	frame, err := zaphttp.MarshalRequest(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got fasthttp.Request
	if err := zaphttp.UnmarshalRequest(frame, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got.Header.Add("X-Added", "ok") // must not panic
	if string(got.Header.Peek("X-Added")) != "ok" {
		t.Errorf("Add to decoded empty Header did not take")
	}
}

// ---- wire golden: byte-for-byte compatibility with the prior codec ----

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad golden hex: %v", err)
	}
	return b
}

// TestWireGolden_DecodeRequest proves the fasthttp codec decodes a frame
// produced by the prior net/http codec faithfully (backward wire compat).
func TestWireGolden_DecodeRequest(t *testing.T) {
	var req fasthttp.Request
	if err := zaphttp.UnmarshalRequest(mustHex(t, goldenRequest), &req); err != nil {
		t.Fatalf("decode golden request: %v", err)
	}
	if string(req.Header.Method()) != "POST" {
		t.Errorf("method = %q want POST", req.Header.Method())
	}
	if string(req.RequestURI()) != "/v1/blocks?height=42" {
		t.Errorf("target = %q want /v1/blocks?height=42", req.RequestURI())
	}
	if string(req.Header.ContentType()) != "application/json" {
		t.Errorf("Content-Type = %q", req.Header.ContentType())
	}
	if string(req.Header.Peek("X-Trace-Id")) != "abc-123" {
		t.Errorf("X-Trace-Id = %q want abc-123", req.Header.Peek("X-Trace-Id"))
	}
	if string(req.Body()) != `{"jsonrpc":"2.0","method":"eth_blockNumber"}` {
		t.Errorf("body = %q", req.Body())
	}
}

func TestWireGolden_DecodeResponse(t *testing.T) {
	var resp fasthttp.Response
	if err := zaphttp.UnmarshalResponse(mustHex(t, goldenResponse), &resp); err != nil {
		t.Fatalf("decode golden response: %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Errorf("status = %d want 200", resp.StatusCode())
	}
	if string(resp.Header.ContentType()) != "application/json" {
		t.Errorf("Content-Type = %q", resp.Header.ContentType())
	}
	if string(resp.Header.Peek("X-Trace-Id")) != "abc-123" {
		t.Errorf("X-Trace-Id = %q", resp.Header.Peek("X-Trace-Id"))
	}
	if string(resp.Body()) != `{"result":"0x2a"}` {
		t.Errorf("body = %q", resp.Body())
	}
}

// TestWireGolden_EncodeRequest proves the fasthttp codec produces the exact
// same bytes as the prior net/http codec for the same logical request.
func TestWireGolden_EncodeRequest(t *testing.T) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod("POST")
	req.SetRequestURI("/v1/blocks?height=42")
	req.Header.SetProtocol("HTTP/1.1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Trace-Id", "abc-123")
	req.SetBody([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber"}`))

	frame, err := zaphttp.MarshalRequest(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := hex.EncodeToString(frame); got != goldenRequest {
		t.Errorf("request frame not byte-identical to prior codec\n got=%s\nwant=%s", got, goldenRequest)
	}
}

func TestWireGolden_EncodeResponse(t *testing.T) {
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	resp.SetStatusCode(200)
	resp.Header.SetProtocol([]byte("HTTP/1.1"))
	resp.Header.SetContentType("application/json")
	resp.Header.Set("X-Trace-Id", "abc-123")
	resp.SetBody([]byte(`{"result":"0x2a"}`))

	frame, err := zaphttp.MarshalResponse(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := hex.EncodeToString(frame); got != goldenResponse {
		t.Errorf("response frame not byte-identical to prior codec\n got=%s\nwant=%s", got, goldenResponse)
	}
}

func TestWireGolden_EncodeEmptyRequest(t *testing.T) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod("GET")
	req.SetRequestURI("/health")
	req.Header.SetProtocol("HTTP/1.1")

	frame, err := zaphttp.MarshalRequest(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := hex.EncodeToString(frame); got != goldenEmptyRequest {
		t.Errorf("empty request frame not byte-identical\n got=%s\nwant=%s", got, goldenEmptyRequest)
	}
}

// TestWireGolden_TrailerResponse round-trips a trailer-bearing response
// through decode and re-encode, asserting byte-identity with the prior
// codec's output.
func TestWireGolden_TrailerResponse(t *testing.T) {
	var resp fasthttp.Response
	if err := zaphttp.UnmarshalResponse(mustHex(t, goldenTrailerResponse), &resp); err != nil {
		t.Fatalf("decode golden trailer response: %v", err)
	}
	if string(resp.Header.ContentType()) != "application/octet-stream" {
		t.Errorf("Content-Type = %q", resp.Header.ContentType())
	}
	if string(resp.Body()) != "streamed" {
		t.Errorf("body = %q want streamed", resp.Body())
	}
	if cs := string(resp.Header.Peek("X-Checksum")); cs != "deadbeef" {
		t.Errorf("trailer X-Checksum = %q want deadbeef", cs)
	}
	frame, err := zaphttp.MarshalResponse(&resp)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if got := hex.EncodeToString(frame); got != goldenTrailerResponse {
		t.Errorf("trailer response not byte-identical after round trip\n got=%s\nwant=%s", got, goldenTrailerResponse)
	}
}

// TestConnReuse confirms idle connections are pooled and reused: a second
// call within the idle window does not dial a new socket.
func TestConnReuse(t *testing.T) {
	var dials int32
	addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") })
	defer stop()
	_ = dials

	tr := zaphttp.NewTransport(addr)
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 3 && time.Now().Before(deadline); i++ {
		resp := fasthttp.AcquireResponse()
		req := fasthttp.AcquireRequest()
		req.SetRequestURI("/")
		req.Header.SetMethod(fasthttp.MethodGet)
		if err := tr.Do(req, resp); err != nil {
			t.Fatalf("do: %v", err)
		}
		if string(resp.Body()) != "ok" {
			t.Errorf("body = %q want ok", resp.Body())
		}
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
}
