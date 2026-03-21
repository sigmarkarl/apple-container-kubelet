BINARY_NAME := apple-container-kubelet
BUILD_DIR := bin
GO := go

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build clean test test-integration test-e2e fmt vet lint install

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/containerd-shim-applevm-v2/

clean:
	rm -rf $(BUILD_DIR)

test:
	$(GO) test -v -count=1 ./config/... ./pkg/...

test-integration:
	$(GO) test -v -count=1 -tags=integration ./test/integration/...

test-e2e:
	$(GO) test -v -count=1 -tags=e2e ./test/e2e/...

install: build
	install -m 755 $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/$(BINARY_NAME)

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint: fmt vet
