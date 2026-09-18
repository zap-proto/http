package http_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/zap-proto/http"
)

// slow answers after hold, so a caller can be cancelled while its read is in
// flight — the one case a read deadline set at the start cannot see coming.
func slow(hold time.Duration) fasthttp.RequestHandler {
	return func(c *fasthttp.RequestCtx) {
		time.Sleep(hold)
		c.SetBodyString("late")
	}
}

func exchange(ctx context.Context, tr *http.Transport, path string) error {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(fasthttp.MethodGet)
	req.SetRequestURI(path)
	return tr.DoContext(ctx, req, resp)
}

// Cancelling the caller stops a read already waiting on the wire, and says why.
func TestDoContextCancelStopsAnInFlightRead(t *testing.T) {
	addr, shutdown := listen(t, slow(5*time.Second))
	defer shutdown()
	tr := http.Dial("tcp", addr)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	err := exchange(ctx, tr, "/slow")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancellation took %v; the read was not interrupted", d)
	}
}

// A deadline shorter than the transport's read timeout wins.
func TestDoContextDeadlineWins(t *testing.T) {
	addr, shutdown := listen(t, slow(5*time.Second))
	defer shutdown()
	tr := http.Dial("tcp", addr) // default read timeout is 30s

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := exchange(ctx, tr, "/slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("deadline took %v to fire", d)
	}
}

// An already-cancelled context never touches the network.
func TestDoContextRefusesADeadContext(t *testing.T) {
	tr := http.Dial("tcp", "127.0.0.1:1") // nothing listens; a dial would fail differently
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exchange(ctx, tr, "/x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// A connection cancellation closed must not go back to the pool: the next call
// on the same transport has to succeed on a fresh one.
func TestDoContextNeverPoolsACancelledConn(t *testing.T) {
	var slowNext = true
	addr, shutdown := listen(t, func(c *fasthttp.RequestCtx) {
		if string(c.Path()) == "/slow" && slowNext {
			time.Sleep(2 * time.Second)
		}
		c.SetBodyString("ok")
	})
	defer shutdown()
	tr := http.Dial("tcp", addr)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_ = exchange(ctx, tr, "/slow")
	cancel()

	if err := exchange(context.Background(), tr, "/fast"); err != nil {
		t.Fatalf("the call after a cancellation failed: %v", err)
	}
}

// Plain Do is DoContext with no deadline: it still completes.
func TestDoIsUnboundedDoContext(t *testing.T) {
	addr, shutdown := listen(t, func(c *fasthttp.RequestCtx) { c.SetBodyString("ok") })
	defer shutdown()
	tr := http.Dial("tcp", addr)
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(fasthttp.MethodGet)
	req.SetRequestURI("/")
	if err := tr.Do(req, resp); err != nil || string(resp.Body()) != "ok" {
		t.Fatalf("Do: err=%v body=%q", err, resp.Body())
	}
}
