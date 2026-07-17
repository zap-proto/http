// Server — a fasthttp.RequestHandler served over ZAP-HTTP.
//
// A Fiber app (or any fasthttp stack) hands its handler straight in:
//
//	srv := &zaphttp.Server{Addr: ":9999", Handler: app.Handler()}
//	srv.ListenAndServe()
//
// The server reads ZAP-HTTP frames off each accepted connection,
// reconstructs a *fasthttp.RequestCtx, dispatches to the handler, and
// writes one response frame back from the ctx's Response. Connections are
// kept alive across requests. There is no net/http anywhere in the path.

package zaphttp

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// Server is a ZAP-HTTP server. Zero-value is usable; common knobs (Addr,
// Handler, ReadTimeout, …) mirror fasthttp.Server.
type Server struct {
	Addr         string                  // ":9999" if empty
	Handler      fasthttp.RequestHandler // required
	ReadTimeout  time.Duration           // 0 means no timeout
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	Logger       fasthttp.Logger // passed to each RequestCtx; nil is fine

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

// ListenAndServe is the convenience equivalent of fasthttp.ListenAndServe.
func ListenAndServe(addr string, handler fasthttp.RequestHandler) error {
	s := &Server{Addr: addr, Handler: handler}
	return s.ListenAndServe()
}

// ListenAndServe binds Addr and serves until Close is called or a fatal
// accept error occurs.
func (s *Server) ListenAndServe() error {
	addr := s.Addr
	if addr == "" {
		addr = ":9999"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve accepts connections on ln and serves each one in its own
// goroutine. Returns when the listener is closed.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	if s.Handler == nil {
		return errors.New("zaphttp: Server.Handler is nil")
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			return err
		}
		go s.serveConn(conn)
	}
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, connReadBufSize)

	// One RequestCtx for the whole connection. fasthttp's own HTTP server pools
	// RequestCtx across requests; the previous ZAP server heap-allocated one per
	// request (plus a fasthttp.Request), which dominated its GC pressure. Init2
	// binds the real conn (so RemoteAddr resolves) and keeps body buffers
	// attached for reuse; Request and Response are reset per request below.
	ctx := &fasthttp.RequestCtx{}
	ctx.Init2(conn, s.Logger, false)

	// Per-connection scratch buffers: the inbound frame is decoded straight into
	// ctx.Request (which copies out what it keeps), and the response frame is
	// built here. Both grow once and are reused, so steady-state serving is
	// allocation-free.
	var readBuf, writeBuf []byte

	for {
		if s.IdleTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
		}
		var err error
		readBuf, err = readFrameInto(br, readBuf)
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosedConn(err) {
				log.Printf("zaphttp: read frame: %v", err)
			}
			return
		}
		if s.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
		}

		// Decode into the connection's RequestCtx (UnmarshalRequest resets it
		// first), then clear the previous request's response and user values —
		// the reset fasthttp.Init would have done for a fresh ctx.
		if err := UnmarshalRequest(readBuf, &ctx.Request); err != nil {
			s.writeError(conn, fasthttp.StatusBadRequest, "malformed request frame: "+err.Error())
			return
		}
		ctx.Response.Reset()
		ctx.ResetUserValues()

		// Defer-recover so a handler panic returns 500 rather than dropping
		// the connection silently.
		func() {
			defer func() {
				if r := recover(); r != nil {
					ctx.Response.Reset()
					ctx.Response.SetStatusCode(fasthttp.StatusInternalServerError)
					log.Printf("zaphttp: handler panic on %s %s: %v",
						ctx.Method(), ctx.RequestURI(), r)
				}
			}()
			s.Handler(ctx)
		}()

		if s.WriteTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(s.WriteTimeout))
		}

		// A streamed body (SSE, MCP notifications, chunked) is delivered as a
		// head frame + N data frames + an end frame, so the client sees each
		// chunk as the handler flushes it — real server→client push over ZAP.
		// A normal body takes the single-frame path.
		if ctx.Response.IsBodyStream() {
			if err := s.streamResponse(conn, &ctx.Response); err != nil {
				if !isClosedConn(err) {
					log.Printf("zaphttp: stream response: %v", err)
				}
				return
			}
			continue
		}

		// Build [4-byte length prefix][response frame] in one buffer and write
		// it with a single syscall.
		writeBuf = append(writeBuf[:0], 0, 0, 0, 0)
		writeBuf, err = AppendResponse(writeBuf, &ctx.Response)
		if err != nil {
			log.Printf("zaphttp: marshal response: %v", err)
			return
		}
		if err := writeFramePrefixed(conn, writeBuf); err != nil {
			if !isClosedConn(err) {
				log.Printf("zaphttp: write frame: %v", err)
			}
			return
		}
	}
}

// connReadBufSize is the per-connection bufio read buffer. Larger than bufio's
// 4 KiB default so back-to-back small frames drain with fewer read syscalls.
const connReadBufSize = 16 << 10

// streamResponse writes a streamed response: the head (status + headers), then
// one data frame per flush of the body stream, then an end frame. BodyWriteTo
// runs the fasthttp body-stream writer against chunkWriter, so each handler
// flush becomes a frame on the wire immediately (no full buffering).
func (s *Server) streamResponse(conn net.Conn, resp *fasthttp.Response) error {
	head, err := MarshalResponseHead(resp)
	if err != nil {
		return err
	}
	if err := writeFrame(conn, head); err != nil {
		return err
	}
	cw := &chunkWriter{w: conn}
	if err := resp.BodyWriteTo(cw); err != nil {
		return err
	}
	if cw.err != nil {
		return cw.err
	}
	return writeFrame(conn, MarshalEnd(nil))
}

// chunkWriter turns each Write (one flush of the body stream) into a FrameData
// frame. The bytes are copied because fasthttp may reuse the buffer.
type chunkWriter struct {
	w   io.Writer
	err error
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	if err := writeFrame(c.w, MarshalData(chunk)); err != nil {
		c.err = err
		return 0, err
	}
	return len(p), nil
}

func (s *Server) writeError(w io.Writer, status int, msg string) {
	var resp fasthttp.Response
	resp.SetStatusCode(status)
	resp.Header.SetContentType("text/plain; charset=utf-8")
	resp.SetBodyString(msg)
	frame, err := MarshalResponse(&resp)
	if err != nil {
		return
	}
	_ = writeFrame(w, frame)
}

// Close stops the listener; in-flight handlers are not interrupted.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}

func isClosedConn(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF)
}
