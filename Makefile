GO ?= go
MODULE_PREP = GOTOOLCHAIN=local $(GO) run ./tools/moduleprep

.PHONY: run-relay run-agent test build docker-build verify-neutral

run-relay:
	$(MODULE_PREP) run -package ./cmd/relay -- $(ARGS)

run-agent:
	$(MODULE_PREP) run -package ./cmd/agent -- $(ARGS)

test:
	$(MODULE_PREP) exec -- $(GO) test ./...
	cd third_party/yamux && GOTOOLCHAIN=local $(GO) test ./...

build:
	mkdir -p bin
	$(MODULE_PREP) exec -- $(GO) build -o $(CURDIR)/bin/relay ./cmd/relay
	$(MODULE_PREP) exec -- $(GO) build -o $(CURDIR)/bin/agent ./cmd/agent

docker-build:
	docker compose build server

verify-neutral:
	GOTOOLCHAIN=local $(GO) run ./tools/neutralcheck
