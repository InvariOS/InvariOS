MODULE   = $(shell $(GO) list -m)
DATE    ?= $(shell date +%FT%T%z)
PKGS     = $(or $(PKG),$(shell $(GO) list ./...))
TESTPKGS = $(shell $(GO) list -f \
			'{{ if or .TestGoFiles .XTestGoFiles }}{{ .ImportPath }}{{ end }}' \
			$(PKGS))
BIN      = $(CURDIR)/bin

GO      = go
GOOS    ?= linux
GOARCH  ?= amd64
TIMEOUT = 15
V = 0
Q = $(if $(filter 1,$V),,@)
M = $(shell printf "\033[34;1m▶\033[0m")

binext=""
ifeq ($(GOOS),windows)
  binext=".exe"
endif

.PHONY: all
all: fmt lint build

.PHONY: build
build: $(BIN) ; $(info $(M) building executable…) @ ## Build program binary
	$Q CGO_ENABLED=0 $(GO) build \
		-ldflags "-X main.gitVersion=$$(git describe --tags) -X $(MODULE)/cmd.gitVersion=$$(git describe --tags) -X \"main.buildTime=$$(date -u '+%Y-%m-%d %H:%M:%S %Z')\" -X \"$(MODULE)/cmd.buildTime=$$(date -u '+%Y-%m-%d %H:%M:%S %Z')\"" \
		-tags release \
		-o $(BIN)/$(notdir $(basename $(MODULE)))$(binext)
# Tools

$(BIN):
	@mkdir -p $@
$(BIN)/%: | $(BIN) ; $(info $(M) building $(PACKAGE)…)
	$Q env GOBIN=$(BIN) $(GO) install $(PACKAGE) \
		|| ret=$$?; \
	   exit $$ret

VULNCHECK = $(BIN)/govulncheck
$(BIN)/govulncheck: PACKAGE=golang.org/x/vuln/cmd/govulncheck@latest

# Tests

TEST_TARGETS := test-verbose test-race
.PHONY: $(TEST_TARGETS) test
test-verbose: ARGS=-v            ## Run tests in verbose mode
test-race:    ARGS=-race         ## Run tests with race detector
$(TEST_TARGETS): NAME=$(MAKECMDGOALS:test-%=%)
$(TEST_TARGETS): test
test: fmt lint vulncheck; $(info $(M) running $(NAME:%=% )tests…) @ ## Run tests
	$Q $(GO) test -timeout $(TIMEOUT)s $(ARGS) $(TESTPKGS)

.PHONY: fmt
fmt: ; $(info $(M) running gofmt…) @ ## Run gofmt on all source files
	$Q $(GO) fmt $(PKGS)

# Run fmt before any linter so parallel `-jN` doesn't change files while a linter is mid flight.
lint vulncheck: fmt

.PHONY: lint
lint: ; $(info $(M) running golangci-lint…) @ ## Run golangci-lint (vet, revive, staticcheck, errcheck)
	$Q GOOS=$(GOOS) GOARCH=$(GOARCH) golangci-lint run ./...

.PHONY: vulncheck
vulncheck: | $(VULNCHECK) ; $(info $(M) running vulncheck…) @
	$Q GOOS=$(GOOS) GOARCH=$(GOARCH) $(VULNCHECK) $(PKGS)

# Misc

.PHONY: clean
clean: ; $(info $(M) cleaning…)	@ ## Cleanup everything
	@rm -rf $(BIN)
	@rm -rf test/tests.*

.PHONY: help
help:
	@grep -hE '^[ a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-17s\033[0m %s\n", $$1, $$2}'