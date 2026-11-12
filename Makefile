.DEFAULT_GOAL=help
SHELL:=/bin/bash

##@ Linting and static checks
.PHONY: vet

vet:  ## Run go vet
	@go vet ./...

##@ Testing

.PHONY: test test-cov-html

test: vet  ## Run tests
	@go test ./... -cover

test-cov-html: vet  ## Run test coverage
	@go test ./... -covermode=count -coverprofile=coverage.out \
	&& go tool cover -html=coverage.out

##@ Helpers

.PHONY: help

# See: https://www.thapaliya.com/en/writings/well-documented-makefiles/
help:  ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
