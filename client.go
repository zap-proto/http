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
// Connection management uses a free-list pool of idle TCP
// connections. RoundTrip pulls an idle conn (or dials a new one),
// uses it for one exchange, and returns it to the pool. The pool
// caps at MaxIdleConns; surplus conns close on return.

package zaphttp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// pooledConn is one idle connection. The bufio.Reader is held with
// the conn so the read-side amortizes its own buffer alloc across
// requests.
type pooledConn struct {
	c  net.Conn
	br *bufio.Reader
}

// Transport implements http.RoundTripper over a ZAP-HTTP TCP
// connection. Field-zero is invalid; use NewTransport.
type Transport struct {
	addr         string
	dialTimeout  time.Duration
	readTimeout  time.Duration
	maxIdleConns int

	mu   sync.Mutex
	idle []*pooledConn // LIFO; most-recently-returned is hottest
}

// NewTransport returns a Transport connecting to the given host:port.
func NewTransport(addr string) *Transport {
	return &Transport{
		addr:         addr,
		dialTimeout:  10 * time.Second,
		readTimeout:  30 * time.Second,
		maxIdleConns: 32,
	}
}

// SetDialTimeout overrides the default 10s dial timeout.
func (t *Transport) SetDialTimeout(d time.Duration) { t.dialTimeout = d }

// SetReadTimeout overrides the default 30s response-read timeout.
func (t *Transport) SetReadTimeout(d time.Duration) { t.readTimeout = d }

// SetMaxIdleConns caps the number of idle conns held in the pool.
// Surplus conns close on return.
func (t *Transport) SetMaxIdleConns(n int) {
	if n < 0 {
		n = 0
	}
	t.maxIdleConns = n
}

// RoundTrip executes a single request/response exchange.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.addr == "" {
		return nil, fmt.Errorf("zaphttp: Transport.addr is empty (use NewTransport)")
	}

	pc, err := t.acquireConn()
	if err != nil {
		return nil, fmt.Errorf("zaphttp: dial %s: %w", t.addr, err)
	}
	conn, br := pc.c, pc.br

	frame, err := MarshalRequest(req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := writeFrame(conn, frame); err != nil {
		conn.Close()
		return nil, fmt.Errorf("zaphttp: write request: %w", err)
	}

	if t.readTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(t.readTimeout))
	}
	respFrame, err := readFrame(br)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("zaphttp: read response: %w", err)
	}
	resp, err := UnmarshalResponse(respFrame)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("zaphttp: unmarshal response: %w", err)
	}

	// Reset the read deadline before returning the conn to the pool.
	_ = conn.SetReadDeadline(time.Time{})

	// The body is already buffered in memory by the unmarshal step,
	// so the conn is reusable as soon as the response is decoded.
	// Wrap Body so the caller can still call Close() without
	// affecting the conn — pool ownership is the transport's
	// responsibility, not the caller's.
	resp.Body = bodyCloser{ReadCloser: resp.Body, release: func() { t.releaseConn(pc) }}
	return resp, nil
}

// bodyCloser wraps the response body so closing it releases the
// underlying TCP connection to the pool.
type bodyCloser struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b bodyCloser) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// acquireConn pops an idle conn or dials a new one.
func (t *Transport) acquireConn() (*pooledConn, error) {
	t.mu.Lock()
	if n := len(t.idle); n > 0 {
		pc := t.idle[n-1]
		t.idle = t.idle[:n-1]
		t.mu.Unlock()
		return pc, nil
	}
	t.mu.Unlock()

	c, err := net.DialTimeout("tcp", t.addr, t.dialTimeout)
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
		_ = tc.SetNoDelay(true)
	}
	return &pooledConn{c: c, br: bufio.NewReader(c)}, nil
}

// releaseConn returns a conn to the pool, or closes it if the pool
// is full.
func (t *Transport) releaseConn(pc *pooledConn) {
	t.mu.Lock()
	if len(t.idle) >= t.maxIdleConns {
		t.mu.Unlock()
		_ = pc.c.Close()
		return
	}
	t.idle = append(t.idle, pc)
	t.mu.Unlock()
}

// CloseIdleConnections closes every idle conn in the pool. Active
// requests are unaffected. Useful in tests + on shutdown.
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	idle := t.idle
	t.idle = nil
	t.mu.Unlock()
	for _, pc := range idle {
		_ = pc.c.Close()
	}
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

// Compile-time guard that the bodyCloser remains a valid io.ReadCloser
// even if the wrapped Body returns a never-closed type.
var _ io.ReadCloser = bodyCloser{}

// errInvalidConn is returned by acquireConn when the pool yields a
// stale connection (currently unused; placeholder for future
// liveness checks).
var errInvalidConn = errors.New("zaphttp: pooled connection invalid")

var _ = errInvalidConn
