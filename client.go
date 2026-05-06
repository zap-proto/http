// Client — net/http.RoundTripper-compatible client speaking ZAP-HTTP.
//
// Drop-in usage:
//
//   t := zaphttp.NewTransport("server:9999")
//   client := &http.Client{Transport: t}
//   resp, err := client.Get("http://server/healthz")
//
// The transport ignores req.URL.Host (use the constructor argument
// to pin the peer) and req.URL.Scheme. Existing http.Client
// machinery — redirects, cookies, retries — works unchanged.
//
// Connection management today is one connection per RoundTrip call.
// v0.2 will introduce a connection pool with keep-alive once the
// underlying ZAP transport ships handshake amortization.

package zaphttp

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Transport implements http.RoundTripper over a ZAP-HTTP TCP
// connection. Field-zero is invalid; use NewTransport.
type Transport struct {
	addr        string
	dialTimeout time.Duration
	readTimeout time.Duration

	mu sync.Mutex
}

// NewTransport returns a Transport connecting to the given host:port.
func NewTransport(addr string) *Transport {
	return &Transport{
		addr:        addr,
		dialTimeout: 10 * time.Second,
		readTimeout: 30 * time.Second,
	}
}

// SetDialTimeout overrides the default 10s dial timeout.
func (t *Transport) SetDialTimeout(d time.Duration) { t.dialTimeout = d }

// SetReadTimeout overrides the default 30s response-read timeout.
func (t *Transport) SetReadTimeout(d time.Duration) { t.readTimeout = d }

// RoundTrip executes a single request/response exchange.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.addr == "" {
		return nil, fmt.Errorf("zaphttp: Transport.addr is empty (use NewTransport)")
	}

	conn, err := net.DialTimeout("tcp", t.addr, t.dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: dial %s: %w", t.addr, err)
	}
	closeOnFail := true
	defer func() {
		if closeOnFail {
			conn.Close()
		}
	}()

	// Marshal + send request.
	frame, err := MarshalRequest(req)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(conn, frame); err != nil {
		return nil, fmt.Errorf("zaphttp: write request: %w", err)
	}

	// Read response.
	if t.readTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(t.readTimeout))
	}
	br := bufio.NewReader(conn)
	respFrame, err := readFrame(br)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: read response: %w", err)
	}
	resp, err := UnmarshalResponse(respFrame)
	if err != nil {
		return nil, fmt.Errorf("zaphttp: unmarshal response: %w", err)
	}

	// Tie the response Body to the connection so it gets closed when
	// the caller closes the body. The current single-frame design
	// means the body is fully buffered in memory, so we can close the
	// connection immediately and leave the buffer in resp.Body. v0.2
	// will defer connection close until body close.
	closeOnFail = false
	conn.Close()
	return resp, nil
}

// Get is a convenience for transports that don't need an http.Client
// wrapper (e.g. service-to-service calls inside a cluster).
func Get(addr, path string) (*http.Response, error) {
	t := NewTransport(addr)
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return nil, err
	}
	return t.RoundTrip(req)
}
