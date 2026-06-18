// End-to-end tests for the ZAP-HTTP wire format and the in-process
// server/client pair. Each test stands one server up on a random
// port, drives it through the public Transport, and asserts the
// response shape.

package zaphttp_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	zaphttp "github.com/zap-proto/http"
)

// listen binds an ephemeral port and starts a server in a goroutine.
// Returns the addr and a shutdown function.
func listen(t *testing.T, handler http.Handler) (addr string, shutdown func()) {
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

// TestRoundTrip_GET — minimum viable exchange.
func TestRoundTrip_GET(t *testing.T) {
	addr, stop := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("server saw method %q want GET", r.Method)
		}
		if r.URL.Path != "/hello" {
			t.Errorf("server saw path %q want /hello", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "hi")
	}))
	defer stop()

	client := &http.Client{Transport: zaphttp.NewTransport(addr)}
	resp, err := client.Get("http://" + addr + "/hello")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hi" {
		t.Errorf("body = %q want %q", body, "hi")
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q want text/plain", got)
	}
}

// TestRoundTrip_POST_bodyEcho — non-empty request body, echoed back
// as the response body.
func TestRoundTrip_POST_bodyEcho(t *testing.T) {
	want := []byte("the quick brown fox jumps over the lazy dog\n")
	addr, stop := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("server read body: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("server got body %q want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(got)
	}))
	defer stop()

	client := &http.Client{Transport: zaphttp.NewTransport(addr)}
	resp, err := client.Post("http://"+addr+"/echo", "application/octet-stream", bytes.NewReader(want))
	if err != nil {
		t.Fatalf("client.Post: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("response body = %q want %q", got, want)
	}
}

// TestRoundTrip_largeBody — 4 MiB binary body, well under MaxFrameSize
// but big enough to exercise the framing path.
func TestRoundTrip_largeBody(t *testing.T) {
	const size = 4 << 20 // 4 MiB
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	addr, stop := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("server read: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("server received corrupted body (len=%d want %d)", len(got), len(payload))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(got)
	}))
	defer stop()

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: zaphttp.NewTransport(addr),
	}
	resp, err := client.Post("http://"+addr+"/big", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("client.Post: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("response body corrupted (len=%d want %d)", len(got), len(payload))
	}
}

// TestHeaders_multiValue — RFC 9110 lets one header name carry many
// values; verify we preserve the list shape end-to-end rather than
// joining with commas.
func TestHeaders_multiValue(t *testing.T) {
	addr, stop := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Values("X-Custom")
		if len(got) != 3 {
			t.Errorf("server got X-Custom values=%v want len=3", got)
		}
		want := []string{"a", "b", "c"}
		for i, v := range want {
			if i < len(got) && got[i] != v {
				t.Errorf("X-Custom[%d] = %q want %q", i, got[i], v)
			}
		}
		w.Header().Add("X-Server-Custom", "x")
		w.Header().Add("X-Server-Custom", "y")
		w.WriteHeader(http.StatusOK)
	}))
	defer stop()

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	req.Header.Add("X-Custom", "a")
	req.Header.Add("X-Custom", "b")
	req.Header.Add("X-Custom", "c")
	resp, err := zaphttp.NewTransport(addr).RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	got := resp.Header.Values("X-Server-Custom")
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("X-Server-Custom = %v want [x y]", got)
	}
}

// TestStatusCode_propagation — non-200 codes round-trip with their
// reason phrase.
func TestStatusCode_propagation(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{http.StatusNotFound, "no such thing"},
		{http.StatusInternalServerError, "boom"},
		{http.StatusServiceUnavailable, "draining"},
		{http.StatusTeapot, "i am a teapot"},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			addr, stop := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, c.body, c.status)
			}))
			defer stop()

			resp, err := zaphttp.Get(addr, "/")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Errorf("status = %d want %d", resp.StatusCode, c.status)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), c.body) {
				t.Errorf("body = %q want to contain %q", body, c.body)
			}
		})
	}
}

// TestKeepAlive — multiple sequential requests on a single TCP
// connection (server side; the v0.1 client opens a new conn per call).
// We verify the server handles N back-to-back requests on the same
// dialed connection by driving the wire layer directly.
func TestKeepAlive(t *testing.T) {
	addr, stop := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("/req-%d", i)
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		frame, err := zaphttp.MarshalRequest(req)
		if err != nil {
			t.Fatalf("marshal req %d: %v", i, err)
		}
		if err := writeAndRead(conn, frame); err != nil {
			t.Fatalf("exchange %d: %v", i, err)
		}
	}
}

// helper: write a length-prefixed frame and read one back.
func writeAndRead(conn net.Conn, frame []byte) error {
	hdr := make([]byte, 4)
	hdr[0] = byte(len(frame) >> 24)
	hdr[1] = byte(len(frame) >> 16)
	hdr[2] = byte(len(frame) >> 8)
	hdr[3] = byte(len(frame))
	if _, err := conn.Write(hdr); err != nil {
		return err
	}
	if _, err := conn.Write(frame); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return err
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	resp := make([]byte, n)
	_, err := io.ReadFull(conn, resp)
	return err
}

// TestMarshalRequest_idempotent — marshal/unmarshal round-trip
// preserves request fields exactly. Pure unit test, no network.
func TestMarshalRequest_idempotent(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	orig, _ := http.NewRequest(http.MethodPost, "http://example.com/foo?a=1", bytes.NewReader(body))
	orig.Header.Set("Content-Type", "application/json")
	orig.Header.Set("Authorization", "Bearer token-stays-encrypted-on-the-wire")

	frame, err := zaphttp.MarshalRequest(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := zaphttp.UnmarshalRequest(frame)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Method != http.MethodPost {
		t.Errorf("method = %q want POST", got.Method)
	}
	if got.URL.Path != "/foo" {
		t.Errorf("path = %q want /foo", got.URL.Path)
	}
	if got.URL.Query().Get("a") != "1" {
		t.Errorf("query a = %q want 1", got.URL.Query().Get("a"))
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type lost")
	}
	if got.Header.Get("Authorization") == "" {
		t.Errorf("Authorization lost")
	}
	gotBody, _ := io.ReadAll(got.Body)
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body mismatch")
	}
}

// TestMarshalResponse_idempotent — marshal/unmarshal round-trip
// preserves response fields exactly, including a reason phrase,
// multi-value headers, a body, and a trailer. Pure unit test, no
// network — the symmetric counterpart to TestMarshalRequest_idempotent
// and the explicit guard on the response frame's field offsets.
func TestMarshalResponse_idempotent(t *testing.T) {
	body := []byte("payload-bytes-\x00\x01\x02-with-nuls")
	orig := &http.Response{
		StatusCode: http.StatusTeapot,
		Status:     "418 I'm a teapot",
		Proto:      "ZAP-HTTP/1.0",
		Header: http.Header{
			"Content-Type": {"application/octet-stream"},
			"X-Multi":      {"one", "two", "three"},
		},
		Body:    io.NopCloser(bytes.NewReader(body)),
		Trailer: http.Header{"X-Checksum": {"deadbeef"}},
	}

	frame, err := zaphttp.MarshalResponse(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := zaphttp.UnmarshalResponse(frame)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d want 418", got.StatusCode)
	}
	if got.Status != "418 I'm a teapot" {
		t.Errorf("status line = %q want %q", got.Status, "418 I'm a teapot")
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q want application/octet-stream", ct)
	}
	if multi := got.Header.Values("X-Multi"); len(multi) != 3 ||
		multi[0] != "one" || multi[1] != "two" || multi[2] != "three" {
		t.Errorf("X-Multi = %v want [one two three]", multi)
	}
	if cs := got.Trailer.Get("X-Checksum"); cs != "deadbeef" {
		t.Errorf("trailer X-Checksum = %q want deadbeef", cs)
	}
	gotBody, _ := io.ReadAll(got.Body)
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body mismatch: got %q want %q", gotBody, body)
	}
}

// TestFrameType_crossGuard — a request frame must not decode as a
// response and vice versa. The frame type rides in the message header
// flags; the decoder rejects a mismatched type rather than silently
// misreading another shape's bytes.
func TestFrameType_crossGuard(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/x", nil)
	reqFrame, err := zaphttp.MarshalRequest(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := zaphttp.UnmarshalResponse(reqFrame); err == nil {
		t.Errorf("UnmarshalResponse accepted a request frame; want error")
	}

	resp := &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}
	respFrame, err := zaphttp.MarshalResponse(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if _, err := zaphttp.UnmarshalRequest(respFrame); err == nil {
		t.Errorf("UnmarshalRequest accepted a response frame; want error")
	}
}

// TestEmptyHeaders_nonNil — a request/response with no headers must
// decode to an empty, non-nil http.Header (the null-slot path), so
// callers can Get/Add without a nil-map panic.
func TestEmptyHeaders_nonNil(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header = nil // force the empty-headers encode path
	frame, err := zaphttp.MarshalRequest(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := zaphttp.UnmarshalRequest(frame)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Header == nil {
		t.Fatal("decoded request Header is nil; want empty non-nil")
	}
	got.Header.Add("X-Added", "ok") // must not panic on a nil map
	if got.Header.Get("X-Added") != "ok" {
		t.Errorf("Add to decoded empty Header did not take")
	}
}
