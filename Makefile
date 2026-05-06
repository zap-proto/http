# zap-http Makefile.
#
# Re-runs Cap'n Proto codegen from schema/zap_http.zap into
# internal/wire/. Requires capnp + capnpc-go on PATH.

GOPATH ?= $(shell go env GOPATH)
SCHEMA := schema/zap_http.zap
GENDIR := internal/wire

.PHONY: schema test build bench clean

schema:
	@command -v capnp >/dev/null || { echo "install capnp: brew install capnp"; exit 1; }
	@command -v $(GOPATH)/bin/capnpc-go >/dev/null || { echo "installing capnpc-go..."; \
		go install capnproto.org/go/capnp/v3/capnpc-go@latest; }
	@mkdir -p $(GENDIR)
	@if [ ! -f schema/go.capnp ]; then \
		gocapnp=$$(find $(GOPATH)/pkg/mod/capnproto.org -name go.capnp 2>/dev/null | head -1); \
		[ -n "$$gocapnp" ] && cp "$$gocapnp" schema/go.capnp; \
	fi
	PATH=$(GOPATH)/bin:$$PATH capnp compile -I schema -ogo:$(GENDIR) $(SCHEMA)

build:
	go build ./...

test:
	go test ./... -v

bench:
	go test ./... -run=^$$ -bench=. -benchmem

clean:
	rm -rf $(GENDIR)
