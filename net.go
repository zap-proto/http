// Copyright © 2026. MIT License.

package http

// net.go — the net/http face of this package.
//
// A caller with net/http code changes its import and nothing else: the types
// below ARE net/http's, by alias, so an existing Handler, Request or
// ResponseWriter satisfies them exactly. Only the transport is different — the
// bytes leave as ZAP frames rather than as HTTP/1.1.
//
// WHY ALIASES AND NOT LOOKALIKES. A declared struct with the same fields is a
// DIFFERENT type: every caller would need a conversion, every middleware would
// stop compiling, and "drop-in" would be a claim rather than a fact. An alias
// is the same type, so it composes with the whole net/http ecosystem —
// httptest, middleware, mux — for free.
//
// WHAT IT COSTS, stated rather than hidden. net/http's Request and
// ResponseWriter allocate where fasthttp's do not, and this package guards a
// zero-alloc encode path. So the fast path stays fasthttp: Server.Handler is
// still a fasthttp.RequestHandler and nothing here is in its way. This file is
// the door for callers who want the standard shapes and will pay for them.

import (
	"net/http"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

// The standard shapes, unchanged. These are net/http's own types.
type (
	Request        = http.Request
	Response       = http.Response
	Header         = http.Header
	Handler        = http.Handler
	HandlerFunc    = http.HandlerFunc
	ResponseWriter = http.ResponseWriter
	ServeMux       = http.ServeMux
)

// NewServeMux returns net/http's mux, so routing composes as usual.
func NewServeMux() *ServeMux { return http.NewServeMux() }

// ListenAndServe serves a net/http Handler over ZAP, exactly as
// net/http.ListenAndServe serves one over HTTP/1.1.
//
// There is ONE of these, and it takes the standard Handler. A second one taking
// fasthttp.RequestHandler used to sit in server.go, which made two ways to start
// a server that differed only in which handler shape you happened to have. A
// caller holding a fasthttp handler composes &Server{Addr, Handler} directly —
// one line, and it says what it is.
func ListenAndServe(addr string, h Handler) error {
	return (&Server{Addr: addr, Handler: Adapt(h)}).ListenAndServe()
}

// Adapt turns a net/http Handler into the fasthttp handler Server takes. It is
// exported because a caller composing a Server itself needs the same bridge,
// and because naming it makes the cost visible at the call site: this is where
// a net/http Request is materialised.
func Adapt(h Handler) fasthttp.RequestHandler {
	return fasthttpadaptor.NewFastHTTPHandler(h)
}
