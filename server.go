// Server — net/http.Handler-compatible server speaking ZAP-HTTP.
//
// Existing handlers work unchanged:
//
//   mux := http.NewServeMux()
//   mux.HandleFunc("/healthz", okHandler)
//   zaphttp.ListenAndServe(":9999", mux)
//
// The server reads ZAP-HTTP frames off each accepted connection,
// reconstructs *http.Request, dispatches to the handler, captures the
// handler's response via a buffering ResponseWriter, and writes one
// response frame back. Connections are kept alive across requests.

package zaphttp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Server is a ZAP-HTTP server. Zero-value is usable; common knobs
// (Addr, Handler, ReadTimeout, …) mirror net/http.Server.
type Server struct {
	Addr           string        // ":9999" if empty
	Handler        http.Handler  // http.DefaultServeMux if nil
	ReadTimeout    time.Duration // 0 means no timeout
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration
	MaxHeaderBytes int           // not enforced today; placeholder for parity

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

// ListenAndServe is the convenience equivalent of net/http.ListenAndServe.
func ListenAndServe(addr string, handler http.Handler) error {
	s := &Server{Addr: addr, Handler: handler}
	return s.ListenAndServe()
}

// ListenAndServe binds Addr and serves until Close is called or a
// fatal accept error occurs.
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

	handler := s.Handler
	if handler == nil {
		handler = http.DefaultServeMux
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
		go s.serveConn(conn, handler)
	}
}

func (s *Server) serveConn(conn net.Conn, handler http.Handler) {
	defer conn.Close()
	br := bufio.NewReader(conn)

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
		req, err := UnmarshalRequest(raw)
		if err != nil {
			s.writeError(conn, http.StatusBadRequest, "malformed request frame: "+err.Error())
			return
		}
		req.RemoteAddr = conn.RemoteAddr().String()

		// Wire a buffering ResponseWriter. The handler runs to
		// completion and we then marshal the captured state into a
		// single response frame. v0.2 will swap this for a streaming
		// writer once the transport supports it.
		rw := &bufferingResponseWriter{
			header: make(http.Header),
			body:   &bytes.Buffer{},
		}
		ctx := context.Background()
		req = req.WithContext(ctx)

		// Defer-recover so a handler panic returns 500 rather than
		// dropping the connection silently.
		func() {
			defer func() {
				if r := recover(); r != nil {
					if !rw.wroteHeader {
						rw.WriteHeader(http.StatusInternalServerError)
					}
					log.Printf("zaphttp: handler panic on %s %s: %v", req.Method, req.URL.RequestURI(), r)
				}
			}()
			handler.ServeHTTP(rw, req)
		}()

		if !rw.wroteHeader {
			rw.WriteHeader(http.StatusOK)
		}
		// Auto-fill Content-Length when the handler hasn't.
		if rw.header.Get("Content-Length") == "" {
			rw.header.Set("Content-Length", strconv.Itoa(rw.body.Len()))
		}

		resp := &http.Response{
			StatusCode: rw.status,
			Status:     fmt.Sprintf("%d %s", rw.status, http.StatusText(rw.status)),
			Header:     rw.header,
			Body:       io.NopCloser(rw.body),
		}
		respBytes, err := MarshalResponse(resp)
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
	resp := &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:       io.NopCloser(bytes.NewBufferString(msg)),
	}
	frame, err := MarshalResponse(resp)
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

// ---- bufferingResponseWriter ----

type bufferingResponseWriter struct {
	header      http.Header
	body        *bytes.Buffer
	status      int
	wroteHeader bool
}

func (w *bufferingResponseWriter) Header() http.Header { return w.header }

func (w *bufferingResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(b)
}

func (w *bufferingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func isClosedConn(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF)
}
