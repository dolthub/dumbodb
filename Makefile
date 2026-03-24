SHELL := /bin/bash

DONGO_BINARY    := $(CURDIR)/.runtime/bin/dongo
RESULTS_FILE    := $(CURDIR)/.runtime/ferretdb-scorecard.txt
COMPAT_FILE     := $(CURDIR)/.runtime/ferretdb-compat.txt

.PHONY: help build ferretdb-scorecard ferretdb-compat

help:
	@echo "Dongo Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build               Build Dongo server binary"
	@echo "  ferretdb-scorecard  Start Dongo, run FerretDB integration tests, report results"
	@echo "  ferretdb-compat     Run FerretDB compat suite: Dongo (target) vs MongoDB (compat)"
	@echo ""
	@echo "ferretdb-compat requires MongoDB running on port 47017."
	@echo "Start it with: cd ferretdb && docker compose up -d mongodb-secure"

build:
	@mkdir -p $(dir $(DONGO_BINARY))
	go build -o $(DONGO_BINARY) ./cmd/dongo/
	@echo "Built: $(DONGO_BINARY)"

ferretdb-scorecard: build
	./scripts/ferretdb-scorecard.sh $(RESULTS_FILE)

ferretdb-compat: build
	./scripts/compat-test.sh $(COMPAT_FILE)
