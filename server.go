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
	Addr         string                    // ":9999" if empty
	Handler      fasthttp.RequestHandler   // required
	ReadTimeout  time.Duration             // 0 means no timeout
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
	br := bufio.NewReader(conn)
	remoteAddr := conn.RemoteAddr()

	for {
		if s.IdleTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
		}
		raw, err := readFrame(br)
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosedConn(err) {
				log.Printf("zaphttp: read frame: %v", err)
			}
			return
		}
		if s.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))
		}

		var req fasthttp.Request
		if err := UnmarshalRequest(raw, &req); err != nil {
			s.writeError(conn, fasthttp.StatusBadRequest, "malformed request frame: "+err.Error())
			return
		}

		// Fresh ctx per request: fasthttp's Init resets connection state and
		// copies the request in, but does not reset a reused Response, so a
		// zero-value ctx is the clean, correct choice.
		ctx := &fasthttp.RequestCtx{}
		ctx.Init(&req, remoteAddr, s.Logger)

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

		respBytes, err := MarshalResponse(&ctx.Response)
		if err != nil {
			log.Printf("zaphttp: marshal response: %v", err)
			return
		}
		if s.WriteTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(s.WriteTimeout))
		}
		if err := writeFrame(conn, respBytes); err != nil {
			if !isClosedConn(err) {
				log.Printf("zaphttp: write frame: %v", err)
			}
			return
		}
	}
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
