package http_test

import (
	nethttp "net/http"
	"testing"

	zap "github.com/zap-proto/http"
)

// A NET/HTTP HANDLER IS THIS PACKAGE'S HANDLER — the same type, not a lookalike.
// If these were separate declarations with matching fields, every caller would
// need a conversion and every middleware would stop compiling, and "drop-in"
// would be a claim instead of a fact. Assigning across proves the alias.
func TestStandardShapesAreTheStandardTypes(t *testing.T) {
	var h zap.Handler = nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {})
	var back nethttp.Handler = h
	if back == nil {
		t.Fatal("a net/http Handler must BE this package's Handler")
	}

	// The mux composes, so routing written against net/http keeps working.
	var mux *nethttp.ServeMux = zap.NewServeMux()
	mux.Handle("/x", h)

	// And the request/response shapes are net/http's own.
	var _ *nethttp.Request = (*zap.Request)(nil)
	var _ nethttp.Header = zap.Header(nil)
}

// The bridge to the fast path exists and is named, so the cost is visible where
// a net/http Request is materialised rather than hidden inside the server.
func TestAdaptProducesTheFastHandler(t *testing.T) {
	if zap.Adapt(nethttp.HandlerFunc(func(nethttp.ResponseWriter, *nethttp.Request) {})) == nil {
		t.Fatal("Adapt returned no handler")
	}
}
