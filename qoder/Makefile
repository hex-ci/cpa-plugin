.PHONY: build test lint clean release

GO ?= go
VERSION ?= $(shell cat VERSION 2>/dev/null || git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X main.version=$(VERSION)

# Default target: build the plugin for the current platform.
build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o qoder.so .

# Run all tests with race detector, plus the panel's JS suite.
test:
	$(GO) test -race -count=1 ./...
	node --test panel.test.js

# Lint everything (requires gofmt + go vet; gocritic/unparam optional).
# staticcheck is pinned: an old binary silently fails to parse the toolchain
# version in go.mod and reports nothing, hiding real findings.
lint:
	@test -z "$$($(GO)fmt -l .)" || ($(GO)fmt -l . && exit 1)
	$(GO) vet ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...
	@if command -v gocritic >/dev/null 2>&1; then gocritic check ./...; fi
	@if command -v unparam >/dev/null 2>&1; then unparam ./...; fi

# Clean build artifacts.
clean:
	rm -f qoder.so qoder.h
	rm -rf bin/ dist/ coverage.out coverage.html

# Cross-compile for all supported platforms (requires cross C toolchains).
# On a Linux host without osxcross, only linux/* targets will succeed.
release: clean
	@mkdir -p dist
	@for target in linux/amd64 linux/arm64; do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  out=dist/qoder_$(VERSION)_$${os}_$${arch}; \
	  mkdir -p $$out; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o $$out/qoder.so . 2>&1 | grep -v "cgo is not enabled" || true; \
	  if [ -f $$out/qoder.so ]; then \
	    cp README.md README_CN.md LICENSE $$out/; \
	    cd dist && zip -qr qoder_$(VERSION)_$${os}_$${arch}.zip qoder_$(VERSION)_$${os}_$${arch} && cd ..; \
	    echo "built $$out"; \
	  fi; \
	done

# Tag a new release (usage: make tag VERSION=0.4.2). Tag format matches the CI release trigger.
tag:
	git tag -a qoder-v$(VERSION) -m "Qoder v$(VERSION)"
	git push origin qoder-v$(VERSION)
