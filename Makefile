# Jan Sahayak — developer and build entry points.
#
#   make build      kiosk UI → embed → bin/device-agent (host)
#   make build-pi   same, cross-compiled for Raspberry Pi 5 (linux/arm64, CGO_ENABLED=0)
#   make test       go test ./... (root module)          make race   with -race
#   make lint       gofmt check + go vet + kiosk lint
#   make dev        start llama-server, speech worker and agent (scripts/dev/up.sh)
#   make down       stop them (scripts/dev/down.sh)
#   make smoke      end-to-end curl smoke test against the running dev stack
#   make soak       10-minute text-session soak against the running dev stack
#   make faultsoak  60-minute fault-injection soak (owns the whole stack; see scripts/dev/faultsoak/run.sh)
#   make upgradetest device DB v1 → current upgrade + rollback test
#
# The speech worker is a separate cgo module (services/speech-worker); it
# cannot be cross-compiled from macOS — build it on the Pi (see
# deployment/raspberry-pi/README.md).

SHELL   := /bin/bash
GO      ?= go
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)-$(shell date -u +%Y%m%d)
LDFLAGS := -s -w -X main.version=$(VERSION)
AGENT   := ./services/device-agent/cmd/device-agent

.PHONY: build build-agent build-pi webui speech-worker test race lint dev down smoke soak faultsoak upgradetest clean

build: webui build-agent

build-agent:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/device-agent $(AGENT)
	@ls -lh bin/device-agent | awk '{print "bin/device-agent", $$5}'

build-pi: webui
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o bin/linux-arm64/device-agent $(AGENT)
	@ls -lh bin/linux-arm64/device-agent | awk '{print "bin/linux-arm64/device-agent", $$5}'

webui:
	scripts/dev/build-webui.sh

speech-worker:
	cd services/speech-worker && $(GO) build -trimpath -o bin/speech-worker ./cmd/speech-worker

test:
	$(GO) test ./...

race:
	$(GO) test -race -p 2 ./...

lint:
	@out=$$(gofmt -l cmd internal services scripts); \
		if [[ -n "$$out" ]]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	pnpm --dir apps/citizen-kiosk lint

dev:
	scripts/dev/up.sh

down:
	scripts/dev/down.sh

smoke:
	scripts/dev/smoke.sh

soak:
	$(GO) run ./scripts/dev/soak -duration 10m -sessions 100 -pid $$(cat var/run/agent.pid)

faultsoak:
	scripts/dev/faultsoak/run.sh -duration 60m

upgradetest:
	scripts/dev/upgradetest/run.sh

clean:
	rm -f bin/device-agent bin/linux-arm64/device-agent
	rm -rf services/device-agent/internal/webui/assets/dist
