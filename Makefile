# See: https://clarkgrubb.com/makefile-style-guide
MAKEFLAGS += --warn-undefined-variables --no-builtin-rules
SHELL := /usr/bin/env bash

# The `--norc` option prevents "PS1: unbound" errors.
# If Bash thinks it is being run with its standard input connected to a network
# connection (such as via SSH or via Docker), it reads and executes commands
# from ~/.bashrc, regardless of whether it thinks it is in interactive mode.
# Bash does not set PS1 in non-interactive environments. But on Ubuntu 24.04 the
# default /etc/bash.bashrc file assumes that PS1 is set.
#
# See https://www.gnu.org/software/bash/manual/bash.html#Invoked-by-remote-shell-daemon
.SHELLFLAGS := --norc -euo pipefail -c
.DEFAULT_GOAL := help
.DELETE_ON_ERROR:
.SUFFIXES:

# Set empty value for MAKECMDGOALS to prevent the "warning: undefined variable 'MAKECMDGOALS'"
# warning from happening when running make without arguments
MAKECMDGOALS ?=

##
# Git and versioning information
##

git_version := $(shell git describe --tags --always --match='v*' --abbrev=14 --dirty)
VERSION ?= $(git_version)

##
# Global vars and dirs
##
bin_dir := _bin

$(bin_dir):
	@mkdir -p $@

go_srcs := $(shell find . -name '*.go' -not -name '*_test.go' -not -path './$(bin_dir)/*')
binary := $(bin_dir)/probe-controller
ldflags := -s -w -X main.version=$(VERSION)

##@ Linting and static checks

.PHONY: lint vet

lint:  ## Run golangci-lint
	@golangci-lint run

vet:  ## Run go vet
	@go vet ./...

##@ Testing

.PHONY: test test-cov-html

test: vet  ## Run tests
	@go test -cover ./...

test-cov-html: vet  ## Run test coverage
	@go test -covermode=count -coverprofile=coverage.out ./... \
	&& go tool cover -html=coverage.out

##@ Build

.PHONY: build bake clean lint-docker

build: $(binary)  ## Build binary locally

$(binary): $(go_srcs) go.mod go.sum | $(bin_dir)
	@CGO_ENABLED=0 go build -ldflags "$(ldflags)" -o $@ main.go

bake: lint-docker  ## Bake Docker image locally
	@VERSION=$(VERSION) docker buildx bake image-local

clean:  ## Clean all temporary files
	@rm -rfv $(bin_dir) coverage.out

lint-docker:  ## Run hadolint
	@hadolint Dockerfile

##@ Helpers

.PHONY: help

# See: https://www.thapaliya.com/en/writings/well-documented-makefiles/
help:  ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
