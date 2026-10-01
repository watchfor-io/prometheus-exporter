VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
BINARY  := watchfor-prometheus-exporter
IMAGE   ?= ghcr.io/watchfor-io/prometheus-exporter

.PHONY: build test vet lint fmt docker run clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(BINARY)

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	test -z "$$(gofmt -l ./cmd ./internal)"

lint: vet fmt
	go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	go run github.com/securego/gosec/v2/cmd/gosec@latest -quiet -exclude=G304 ./...

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg REVISION=$$(git rev-parse HEAD 2>/dev/null) -t $(IMAGE):$(VERSION) .

# Quick local run: make run WATCHFOR_API_KEY=wf_live_...
run: build
	./$(BINARY)

clean:
	rm -f $(BINARY)
	rm -rf dist
