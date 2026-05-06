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
