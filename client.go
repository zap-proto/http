// Client — a fasthttp-style client speaking ZAP-HTTP.
//
// Usage mirrors fasthttp's Do(req, resp):
//
//	t := zaphttp.NewTransport("server:9999")
//	req := fasthttp.AcquireRequest()
//	resp := fasthttp.AcquireResponse()
//	req.SetRequestURI("/healthz")
//	req.Header.SetMethod(fasthttp.MethodGet)
//	err := t.Do(req, resp)
//
// Connection management uses a free-list pool of idle TCP connections. Do
// pulls an idle conn (or dials a new one), runs one exchange, and returns
// it to the pool. Because UnmarshalResponse fully buffers the response
// body into resp, the connection is reusable the instant the frame is
// decoded — no body-close handshake is needed. The pool caps at
// MaxIdleConns; surplus conns close on return.

package zaphttp

import (
	"bufio"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// pooledConn is one idle connection. The bufio.Reader is held with the
// conn so the read side amortizes its own buffer alloc across requests.
type pooledConn struct {
	c  net.Conn
	br *bufio.Reader
}

// Transport speaks ZAP-HTTP over a pooled TCP connection. Field-zero is
// invalid; use NewTransport.
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

// SetMaxIdleConns caps the number of idle conns held in the pool. Surplus
// conns close on return.
func (t *Transport) SetMaxIdleConns(n int) {
	if n < 0 {
		n = 0
	}
	t.maxIdleConns = n
}

// Do executes a single request/response exchange, filling resp. It is safe
// for concurrent use: each call takes its own connection from the pool.
func (t *Transport) Do(req *fasthttp.Request, resp *fasthttp.Response) error {
	if t.addr == "" {
		return fmt.Errorf("zaphttp: Transport.addr is empty (use NewTransport)")
	}

	pc, err := t.acquireConn()
	if err != nil {
		return fmt.Errorf("zaphttp: dial %s: %w", t.addr, err)
	}
	conn, br := pc.c, pc.br

	frame, err := MarshalRequest(req)
	if err != nil {
		conn.Close()
		return err
	}
	if err := writeFrame(conn, frame); err != nil {
		conn.Close()
		return fmt.Errorf("zaphttp: write request: %w", err)
	}

	if t.readTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(t.readTimeout))
	}
	respFrame, err := readFrame(br)
	if err != nil {
		conn.Close()
		return fmt.Errorf("zaphttp: read response: %w", err)
	}
	if err := UnmarshalResponse(respFrame, resp); err != nil {
		conn.Close()
		return fmt.Errorf("zaphttp: unmarshal response: %w", err)
	}

	// Reset the read deadline and return the conn to the pool. The response
	// is fully buffered in resp, so the conn is immediately reusable.
	_ = conn.SetReadDeadline(time.Time{})
	t.releaseConn(pc)
	return nil
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

// releaseConn returns a conn to the pool, or closes it if the pool is full.
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

// CloseIdleConnections closes every idle conn in the pool. Active requests
// are unaffected. Useful in tests and on shutdown.
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	idle := t.idle
	t.idle = nil
	t.mu.Unlock()
	for _, pc := range idle {
		_ = pc.c.Close()
	}
}

// Get is a convenience for one-shot service-to-service calls. The caller
// owns resp and must fasthttp.ReleaseResponse it when done.
func Get(addr, path string) (*fasthttp.Response, error) {
	t := NewTransport(addr)
	defer t.CloseIdleConnections()

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.Header.SetMethod(fasthttp.MethodGet)
	req.SetRequestURI(path)
	req.Header.SetHost(addr)

	resp := fasthttp.AcquireResponse()
	if err := t.Do(req, resp); err != nil {
		fasthttp.ReleaseResponse(resp)
		return nil, err
	}
	return resp, nil
}
