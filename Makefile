SHELL = /bin/bash

# /tmp est monté noexec dans certains environnements ; garder le tmp d'exec de go sous $HOME.
export GOTMPDIR ?= $(HOME)/.gotmp

.DEFAULT_GOAL := help
.PHONY: help test lint

help:
	@echo "Cibles :"
	@echo "  make test  Lance les tests"
	@echo "  make lint  golangci-lint run ./..."

test:
	@mkdir -p $(GOTMPDIR)
	go test ./...

lint:
	@mkdir -p $(GOTMPDIR)
	golangci-lint run ./...
