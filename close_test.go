package http_test

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/zap-proto/http"
)

// ticker is a streamed body that never ends on its own and records its Close.
type ticker struct{ closed atomic.Bool }

func (t *ticker) Read(p []byte) (int, error) {
	time.Sleep(5 * time.Millisecond)
	return copy(p, "tick\n"), nil
}

func (t *ticker) Close() error { t.closed.Store(true); return nil }

// TestStream_HangupClosesTheBody: a caller that leaves before the stream's head
// is written ends the connection, and the body the handler streamed is closed
// with it. A handler holds resources until its stream is closed — a proxy holds
// the upstream it is relaying — so a body left open when the connection dies is
// a leak that nothing else will ever release.
func TestStream_HangupClosesTheBody(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	body := &ticker{}
	answer := make(chan struct{})
	srv := &http.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		<-answer // the caller has gone by the time the head is written
		ctx.Response.SetBodyStream(body, -1)
	}}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	tr := http.Dial("unix", sock)
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod("GET")
	req.SetRequestURI("/feed")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tr.DoContext(ctx, req, resp); close(done) }()
	time.Sleep(50 * time.Millisecond) // the request frame is on the wire
	cancel()                          // the caller hangs up
	<-done
	close(answer)

	deadline := time.Now().Add(3 * time.Second)
	for !body.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the caller left and the streamed body was never closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
