SHELL := /bin/bash

DONGO_BINARY           := $(CURDIR)/.runtime/bin/dongo
RESULTS_FILE           := $(CURDIR)/.runtime/ferretdb-scorecard.txt
COMPAT_FILE            := $(CURDIR)/.runtime/ferretdb-compat.txt
MONGODB_REFERENCE_FILE := $(CURDIR)/.runtime/mongodb-reference.txt
FERRETDB_REFERENCE_FILE := $(CURDIR)/.runtime/ferretdb-reference.txt

.PHONY: help build ferretdb-scorecard ferretdb-compat mongodb-reference ferretdb-reference bats

help:
	@echo "Dongo Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build                Build Dongo server binary"
	@echo "  bats                 Run bats integration tests (tests/bats/)"
	@echo "  ferretdb-scorecard   Start Dongo, run FerretDB integration tests, report results"
	@echo "  ferretdb-compat      Run FerretDB compat suite: Dongo (target) vs MongoDB (compat)"
	@echo "  mongodb-reference    Run FerretDB suite against real MongoDB (baseline)"
	@echo "  ferretdb-reference   Run FerretDB suite against FerretDB itself (baseline)"
	@echo ""
	@echo "ferretdb-compat requires MongoDB (with auth) running on port 47017."
	@echo "Start it with: cd ferretdb && docker compose up -d mongodb-secure"
	@echo ""
	@echo "mongodb-reference requires MongoDB (no auth) running on port 37017."
	@echo "Start it with: cd ferretdb && docker compose up -d mongodb"
	@echo ""
	@echo "ferretdb-reference requires FerretDB running on port 27018 (default)."
	@echo "Or set POSTGRESQL_URL to use in-process FerretDB."
	@echo "See scripts/ferretdb-reference.sh for setup instructions."

build:
	@mkdir -p $(dir $(DONGO_BINARY))
	go build -o $(DONGO_BINARY) ./cmd/dongo/
	@echo "Built: $(DONGO_BINARY)"

ferretdb-scorecard: build
	./scripts/ferretdb-scorecard.sh $(RESULTS_FILE)

ferretdb-compat: build
	./scripts/compat-test.sh $(COMPAT_FILE)

mongodb-reference:
	./scripts/mongodb-reference.sh $(MONGODB_REFERENCE_FILE)

ferretdb-reference:
	./scripts/ferretdb-reference.sh $(FERRETDB_REFERENCE_FILE)

bats: build
	bats tests/bats/
