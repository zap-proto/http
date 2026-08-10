package zaphttp

import (
	"testing"

	"github.com/valyala/fasthttp"
)

// THE HOST REACHES THE CALLEE. It is the only thing telling a callee which of
// several brands was asked for, and this wire used to drop it: the encoder
// excluded Host as "frame-owned" while nothing in the frame carried one, and the
// decoder reconstructed it from the very header the encoder had removed. Every
// request arrived with an empty Host, invisibly, because a callee that serves
// one brand never reads it.
func TestHostSurvivesTheWire(t *testing.T) {
	in := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(in)
	in.Header.SetMethod(fasthttp.MethodGet)
	in.Header.SetRequestURI("/v1/iam/.well-known/openid-configuration")
	in.Header.SetHost("lux.id")

	var enc []byte
	enc = appendRequestHeaders(enc, &in.Header, (*trailerSkip)(nil))

	out := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(out)
	if err := decodeRequestHeaders(&out.Header, enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(out.Header.Host()); got != "lux.id" {
		t.Fatalf("callee saw Host %q, want \"lux.id\" — a multi-brand callee cannot answer as the brand it was asked as", got)
	}
}
