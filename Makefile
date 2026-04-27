SHELL := /bin/bash

DUMBODB_BINARY := $(CURDIR)/.runtime/bin/dumbodb

.PHONY: help build test bats

help:
	@echo "DumboDB Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build   Build DumboDB server binary"
	@echo "  test    Run Go tests (go test ./...)"
	@echo "  bats    Run bats integration tests (tests/bats/)"

build:
	@mkdir -p $(dir $(DUMBODB_BINARY))
	go build -o $(DUMBODB_BINARY) ./cmd/dumbodb/
	@echo "Built: $(DUMBODB_BINARY)"

test:
	go test ./... -count=1

bats: build
	bats tests/bats/
