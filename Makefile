GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

IMAGE          ?= ghcr.io/oni-giri/browser-pier
CHROMIUM_IMAGE ?= ghcr.io/oni-giri/browser-pier-chromium

.PHONY: all build run test test-docker vet lint fmt release-files image image-chromium push push-chromium clean

all: lint test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/browserpier ./cmd/browserpier

run: build
	./bin/browserpier

test:
	$(GO) test -race ./...

# Tests that need a Docker daemon are tagged `//go:build docker`. They run
# against the browser image, so build it first.
test-docker: image-chromium
	BROWSERPIER_TEST_IMAGE=$(CHROMIUM_IMAGE):$(VERSION) $(GO) test -race -tags docker -timeout 20m -v ./...

# The files a release of CHANGELOG.md's newest version would attach, in
# dist/, checked as a compose file when Docker Compose is here.
release-files:
	rm -rf dist
	deploy/release-files.sh $$(awk '/^## /{print $$2; exit}' CHANGELOG.md) dist
	! docker compose version >/dev/null 2>&1 || docker compose -f dist/docker-compose.yml --env-file dist/env.example config >/dev/null

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

image-chromium:
	docker build -f images/chromium/Dockerfile -t $(CHROMIUM_IMAGE):$(VERSION) .

push: image
	docker push $(IMAGE):$(VERSION)

push-chromium: image-chromium
	docker push $(CHROMIUM_IMAGE):$(VERSION)

clean:
	rm -rf bin/ dist/
