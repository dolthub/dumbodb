SHELL := /bin/bash

DUMBODB_BINARY := $(CURDIR)/.runtime/bin/dumbodb
SOAK_BINARY := $(CURDIR)/.runtime/bin/soak
REMOTESRV_BINARY := $(CURDIR)/.runtime/bin/remotesrv
REMOTESRV_PKG := github.com/dolthub/dolt/go/utils/remotesrv
GIT_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/dolthub/dumbodb/internal/version.GitVersion=$(GIT_VERSION)

.PHONY: help build soak remotesrv test bats

help:
	@echo "DumboDB Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build   Build DumboDB server binary"
	@echo "  soak    Build the soak load-test binary"
	@echo "  test    Run Go tests (go test ./...)"
	@echo "  bats    Run bats integration tests (tests/bats/)"

build:
	@mkdir -p $(dir $(DUMBODB_BINARY))
	go build -ldflags "$(LDFLAGS)" -o $(DUMBODB_BINARY) ./cmd/dumbodb/
	@echo "Built: $(DUMBODB_BINARY) ($(GIT_VERSION))"

soak:
	@mkdir -p $(dir $(SOAK_BINARY))
	go build -ldflags "$(LDFLAGS)" -o $(SOAK_BINARY) ./cmd/soak/
	@echo "Built: $(SOAK_BINARY) ($(GIT_VERSION))"

# Builds the Dolt remotesrv from the pinned dolt module so remote-sync bats
# tests have a local push/fetch/clone endpoint matching dumbodb's dolt version.
remotesrv:
	@mkdir -p $(dir $(REMOTESRV_BINARY))
	go build -o $(REMOTESRV_BINARY) $(REMOTESRV_PKG)
	@echo "Built: $(REMOTESRV_BINARY)"

test:
	go test ./... -count=1

bats: build remotesrv
	bats tests/bats/
