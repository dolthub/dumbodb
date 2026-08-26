SHELL := /bin/bash

DUMBODB_BINARY := $(CURDIR)/.runtime/bin/dumbodb
SOAK_BINARY := $(CURDIR)/.runtime/bin/soak
REMOTESRV_BINARY := $(CURDIR)/.runtime/bin/remotesrv
REMOTESRV_PKG := github.com/dolthub/dolt/go/utils/remotesrv
MINIO_BINARY := $(CURDIR)/.runtime/bin/minio
MINIO_VERSION := RELEASE.2025-09-07T16-13-09Z
MINIO_URL := https://dl.min.io/server/minio/release/linux-amd64/archive/minio.$(MINIO_VERSION)
FAKEGCS_BINARY := $(CURDIR)/.runtime/bin/fake-gcs-server
FAKEGCS_VERSION := v1.52.2
GIT_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/dolthub/dumbodb/internal/version.GitVersion=$(GIT_VERSION)

.PHONY: help build soak remotesrv minio fakegcs test bats

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

# Downloads the MinIO server binary (linux-amd64) for the s3:// remote-sync bats
# tests. Kept out of the default `bats` target so it stays offline; the s3 bats
# test skips when this binary is absent. Run `make minio` to enable it.
minio:
	@mkdir -p $(dir $(MINIO_BINARY))
	@if [ ! -x "$(MINIO_BINARY)" ]; then \
		echo "Downloading minio $(MINIO_VERSION)..."; \
		curl -fsSL -o "$(MINIO_BINARY)" "$(MINIO_URL)" && chmod +x "$(MINIO_BINARY)"; \
	fi
	@echo "minio: $(MINIO_BINARY)"

# Builds the fake-gcs-server emulator for the gs:// remote-sync bats tests. Like
# `minio`, kept out of the default `bats` target; the gs test skips when it is
# absent. In CI a Docker service container can stand in for this binary.
fakegcs:
	@mkdir -p $(dir $(FAKEGCS_BINARY))
	@if [ ! -x "$(FAKEGCS_BINARY)" ]; then \
		echo "Installing fake-gcs-server $(FAKEGCS_VERSION)..."; \
		GOBIN=$(dir $(FAKEGCS_BINARY)) go install github.com/fsouza/fake-gcs-server@$(FAKEGCS_VERSION); \
	fi
	@echo "fake-gcs-server: $(FAKEGCS_BINARY)"

test:
	go test ./... -count=1

bats: build remotesrv
	bats tests/bats/
