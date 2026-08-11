package http_test

import (
	nethttp "net/http"
	"testing"

	zap "github.com/zap-proto/http"
)

// The alias holds: a net/http Handler IS this package's Handler. Assigning
// across is the proof; matching fields would not be.
func TestStandardShapesAreTheStandardTypes(t *testing.T) {
	var h zap.Handler = nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {})
	var back nethttp.Handler = h
	if back == nil {
		t.Fatal("a net/http Handler must BE this package's Handler")
	}

	// So the mux composes.
	var mux *nethttp.ServeMux = zap.NewServeMux()
	mux.Handle("/x", h)

	var _ *nethttp.Request = (*zap.Request)(nil)
	var _ nethttp.Header = zap.Header(nil)
}

// The bridge is named, so the allocation has a visible home.
func TestAdaptProducesTheFastHandler(t *testing.T) {
	if zap.Adapt(nethttp.HandlerFunc(func(nethttp.ResponseWriter, *nethttp.Request) {})) == nil {
		t.Fatal("Adapt returned no handler")
	}
}
