# torrent-tracker - build the Svelte UI, embed it, and build the Go binary.

BINARY  := trackerd
DB      ?= trackers.db
ADDR    ?= :8080
LIST    ?= list.txt

# CGO is off: modernc.org/sqlite is pure Go, so the binary is static and
# cross-compiles without a C toolchain.
export CGO_ENABLED = 0

GO_SRC  := $(shell find . -name '*.go' -not -path './web/node_modules/*')
# Test files are not bundled, so they must not trigger a UI rebuild.
WEB_SRC := $(shell find web/src -type f -not -name '*.test.js' 2>/dev/null) \
           web/index.html web/vite.config.js
UI      := web/dist/index.html

.PHONY: all build ui test test-go test-ui vet fmt tidy check vuln clean distclean run poll import list changes dev install help

all: build ## Build the UI and the binary (default)

## --- build ---------------------------------------------------------------

build: $(BINARY) ## Build the binary with the UI embedded

$(BINARY): $(GO_SRC) go.mod $(UI)
	go build -ldflags '-s -w' -o $@ ./cmd/trackerd

ui: $(UI) ## Build the Svelte frontend into web/dist

$(UI): web/node_modules $(WEB_SRC)
	cd web && npm run build

web/node_modules: web/package.json
	cd web && npm install
	@touch $@

install: $(UI) ## go install the binary into GOBIN
	go install -ldflags '-s -w' ./cmd/trackerd

## --- quality -------------------------------------------------------------

test: test-go test-ui ## Run both test suites

test-go: $(UI) ## Run the Go test suite
	go test ./...

test-ui: web/node_modules ## Run the frontend test suite
	cd web && npm test

vet: $(UI) ## Run go vet
	go vet ./...

fmt: ## Format Go sources
	gofmt -w $(GO_SRC)

tidy: ## Tidy go.mod
	go mod tidy

check: fmt vet test ## Format, vet and test

# Not part of check: it fetches the vulnerability database over the network.
vuln: ## Report advisories this code can actually reach
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## --- running -------------------------------------------------------------

run: build ## Serve the API and UI on $(ADDR), collecting hourly
	./$(BINARY) --db $(DB) serve --addr $(ADDR)

poll: build ## Run one collection pass and exit
	./$(BINARY) --db $(DB) poll

import: build ## Import announce URLs from $(LIST)
	./$(BINARY) --db $(DB) import --file $(LIST)

list: build ## List known trackers
	./$(BINARY) --db $(DB) list

changes: build ## Show the recent change feed
	./$(BINARY) --db $(DB) changes

dev: ## Vite dev server with hot reload (run `make run` in another shell)
	cd web && npm run dev

## --- cleaning ------------------------------------------------------------

clean: ## Remove build output
	rm -f $(BINARY)
	rm -rf web/dist

distclean: clean ## Also remove node_modules and the database
	rm -rf web/node_modules
	rm -f $(DB) $(DB)-wal $(DB)-shm

help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
