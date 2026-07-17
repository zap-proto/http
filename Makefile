# zap-http Makefile.
#
# The ZAP-HTTP wire codec is hand-written in wire.go against the
# github.com/zap-proto/go runtime — there is no code-generation step and
# no external schema toolchain. schema/zap_http.zap documents the
# logical wire shape; wire.go's field offsets are the binding.

.PHONY: build test bench clean

build:
	go build ./...

test:
	go test ./... -v

bench:
	go test ./... -run=^$$ -bench=. -benchmem

clean:
	go clean ./...
