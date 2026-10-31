GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

IMAGE          ?= ghcr.io/oni-giri/browser-pier
CHROMIUM_IMAGE ?= ghcr.io/oni-giri/browser-pier-chromium

.PHONY: all build run test test-docker vet lint fmt image push clean

all: lint test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/browserpier ./cmd/browserpier

run: build
	./bin/browserpier

test:
	$(GO) test -race ./...

# Tests that need a Docker daemon are tagged `//go:build docker`.
test-docker:
	$(GO) test -race -tags docker ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

push: image
	docker push $(IMAGE):$(VERSION)

clean:
	rm -rf bin/
