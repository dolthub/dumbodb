SHELL := /bin/bash

DONGO_BINARY := $(CURDIR)/.runtime/bin/dongo
RESULTS_FILE := $(CURDIR)/.runtime/ferretdb-scorecard.txt

.PHONY: help build ferretdb-scorecard

help:
	@echo "Dongo Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build               Build Dongo server binary"
	@echo "  ferretdb-scorecard  Start Dongo, run FerretDB integration tests, report results"

build:
	@mkdir -p $(dir $(DONGO_BINARY))
	go build -o $(DONGO_BINARY) ./cmd/dongo/
	@echo "Built: $(DONGO_BINARY)"

ferretdb-scorecard: build
	./scripts/ferretdb-scorecard.sh $(RESULTS_FILE)
