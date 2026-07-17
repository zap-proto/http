// Minimal zap-http example: server + client in one binary.
//
//	go run ./examples/hello server :9999
//	go run ./examples/hello client :9999
//
// The server speaks ZAP-HTTP on :9999 with a fasthttp handler. The client
// dials, issues GET /hello, and prints the response body.

package main

import (
	"fmt"
	"os"

	"github.com/valyala/fasthttp"
	zaphttp "github.com/zap-proto/http"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: %s [server|client] addr\n", os.Args[0])
		os.Exit(2)
	}
	role, addr := os.Args[1], os.Args[2]

	switch role {
	case "server":
		handler := func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set("Content-Type", "text/plain")
			fmt.Fprintf(ctx, "hello from %s — you said %s %s\n", addr, ctx.Method(), ctx.Path())
		}
		fmt.Printf("zap-http server listening on %s\n", addr)
		if err := zaphttp.ListenAndServe(addr, handler); err != nil {
			fmt.Fprintln(os.Stderr, "server:", err)
			os.Exit(1)
		}
	case "client":
		resp, err := zaphttp.Get(addr, "/hello")
		if err != nil {
			fmt.Fprintln(os.Stderr, "client:", err)
			os.Exit(1)
		}
		defer fasthttp.ReleaseResponse(resp)
		fmt.Printf("← %d %s\n", resp.StatusCode(), fasthttp.StatusMessage(resp.StatusCode()))
		os.Stdout.Write(resp.Body())
	default:
		fmt.Fprintln(os.Stderr, "role must be 'server' or 'client'")
		os.Exit(2)
	}
}
