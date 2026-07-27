package zaphttp_test

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"

	zaphttp "github.com/zap-proto/http"
)

// TestStream_SSEOverZAP proves server→client PUSH over ZAP: an SSE handler that
// flushes N events incrementally is received as a live stream by the client,
// chunk-by-chunk (each flush a FrameData frame), not one buffered blob. This is
// the bidirectional-streaming brick that makes MCP notifications / progress /
// chunked bodies real over the ZAP transport.
func TestStream_SSEOverZAP(t *testing.T) {
	const n = 3
	release := make(chan struct{}, n) // test controls when each event flushes

	addr, stop := listen(t, func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Content-Type", "text/event-stream")
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			for i := 0; i < n; i++ {
				<-release // block until the test lets this event go
				_, _ = w.WriteString("data: event-")
				_, _ = w.WriteString(string(rune('0' + i)))
				_, _ = w.WriteString("\n\n")
				_ = w.Flush() // one flush -> one frame on the wire
			}
		})
	})
	defer stop()

	tr := zaphttp.Dial("tcp", addr)
	defer tr.CloseIdleConnections()
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod("GET")
	req.SetRequestURI("/events")

	if err := tr.Do(req, resp); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if ct := string(resp.Header.ContentType()); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream (head frame carried headers)", ct)
	}
	if !resp.IsBodyStream() {
		t.Fatal("response body is not a stream — server sent a buffered response, not a head+chunks stream")
	}

	// Read the body stream incrementally, releasing one event at a time and
	// asserting it arrives BEFORE the next is released — i.e. real push, not a
	// buffered dump at end-of-handler.
	br := bufio.NewReader(resp.BodyStream())
	for i := 0; i < n; i++ {
		release <- struct{}{}
		line, err := readEvent(t, br)
		if err != nil {
			t.Fatalf("event %d read: %v", i, err)
		}
		want := "data: event-" + string(rune('0'+i))
		if !strings.Contains(line, want) {
			t.Fatalf("event %d = %q, want %q", i, line, want)
		}
	}
}

// readEvent reads until a blank-line-terminated SSE event, with a timeout so a
// buffering regression (nothing arrives until the handler returns) fails fast.
func readEvent(t *testing.T, br *bufio.Reader) (string, error) {
	t.Helper()
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var b strings.Builder
		for {
			line, err := br.ReadString('\n')
			b.WriteString(line)
			if err != nil {
				ch <- res{b.String(), err}
				return
			}
			if line == "\n" && b.Len() > 1 {
				ch <- res{b.String(), nil}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(2 * time.Second):
		return "", errTimeout
	}
}

var errTimeout = &timeoutErr{}

type timeoutErr struct{}

func (*timeoutErr) Error() string {
	return "timed out waiting for streamed event (buffering regression?)"
}
