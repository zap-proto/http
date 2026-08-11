// Copyright © 2026. MIT License.

package http

// The net/http face of this package: an existing handler runs unchanged, over
// ZAP frames instead of HTTP/1.1.

import (
	"net/http"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

// Aliases, not lookalikes — a struct with the same fields would be a different
// type, and every caller would need a conversion.
type (
	Request        = http.Request
	Response       = http.Response
	Header         = http.Header
	Handler        = http.Handler
	HandlerFunc    = http.HandlerFunc
	ResponseWriter = http.ResponseWriter
	ServeMux       = http.ServeMux
)

func NewServeMux() *ServeMux { return http.NewServeMux() }

// ListenAndServe serves h over ZAP. A caller holding a fasthttp handler builds
// &Server{Addr, Handler} instead.
func ListenAndServe(addr string, h Handler) error {
	return (&Server{Addr: addr, Handler: Adapt(h)}).ListenAndServe()
}

// Adapt bridges to the fasthttp handler Server takes. Exported so the cost is
// visible where it lands: a net/http Request allocates, fasthttp's does not.
func Adapt(h Handler) fasthttp.RequestHandler {
	return fasthttpadaptor.NewFastHTTPHandler(h)
}
