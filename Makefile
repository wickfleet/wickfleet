.DEFAULT_GOAL := help
SHELL := bash

IMAGE   ?= wickfleet-hub
GOVULNCHECK ?= v1.8.0
PNPM    ?= npx -y pnpm@11.25.0
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

# Optional local additions to the targets below.
-include private.mk

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nusage: make <target>\n"} \
	  /^[a-zA-Z0-9_.-]+:.*##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: setup
setup: go-setup $(PRIVATE_SETUP) ## Go modules (and the player app's packages)

.PHONY: build
build: go-build $(PRIVATE_BUILD) ## The binaries in bin/ (and the player app in player/dist)

.PHONY: test
test: go-test $(PRIVATE_TEST) ## Go tests (and the player app's unit tests)

.PHONY: lint
lint: go-lint $(PRIVATE_LINT) ## Go linters (and the player app's lint and type checks)

.PHONY: fmt
fmt: go-fmt $(PRIVATE_FMT) ## Rewrite Go files (and player app files) in place

##@ Go

.PHONY: go-setup
go-setup: ## Download Go modules
	go mod download

.PHONY: go-build
go-build: ## Build bin/wickfleet-hub, bin/wickfleet-strategy and bin/wickfleet-dockerguard
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/wickfleet-hub ./cmd/wickfleet-hub
	CGO_ENABLED=0 go build -trimpath -o bin/wickfleet-strategy ./cmd/wickfleet-strategy
	CGO_ENABLED=0 go build -trimpath -o bin/wickfleet-dockerguard ./cmd/wickfleet-dockerguard

.PHONY: go-test
go-test: ## Unit tests with the race detector and coverage
	go test -race -coverprofile=coverage.out ./...

.PHONY: go-lint
go-lint: ## gofmt check, go vet, go mod tidy check
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go mod tidy -diff

.PHONY: vuln
vuln: ## Known-vulnerability scan of reachable code (govulncheck)
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK) ./...

.PHONY: go-fmt
go-fmt: ## Rewrite Go files with gofmt
	gofmt -w .

FREQTRADE_IMAGE ?= freqtradeorg/freqtrade:2026.9

# The generated AllKinds strategy runs in the freqtrade image; the Go indicators are tested against its output.
.PHONY: strategy-golden
strategy-golden: ## Regenerate the indicator golden values with freqtrade's TA-Lib and pandas (needs docker)
	tmp="$$(mktemp -d)" && go run ./cmd/wickfleet-strategy gen internal/strategy/testdata/AllKinds.json > "$$tmp/AllKinds.py" && \
	  cp internal/strategy/testdata/golden.py "$$tmp/" && chmod -R a+rX "$$tmp" && \
	  docker run --rm --entrypoint python -v "$$tmp:/w:ro" $(FREQTRADE_IMAGE) /w/golden.py > "$$tmp/out.json" && \
	  mv "$$tmp/out.json" internal/strategy/testdata/columns.golden.json; status=$$?; rm -rf "$$tmp"; exit $$status

# Each demo spec against its hand-written twin in examples/equivalence: same trades, more than zero.
.PHONY: equivalence
equivalence: ## Backtest the example specs against their hand-written twins, trade for trade (needs docker, network)
	scripts/equivalence examples/registry/strategies/EmaCross.json examples/equivalence/EmaCrossHand.py
	scripts/equivalence examples/registry/strategies/RsiReversion.json examples/equivalence/RsiReversionHand.py

##@ Run locally

.PHONY: run
run: go-build ## Run the hub locally against ./dev (registry/ and secrets/)
	WICKFLEET_LISTEN=127.0.0.1:8090 WICKFLEET_REGISTRY_DIR=dev/registry WICKFLEET_SECRETS_DIR=dev/secrets bin/wickfleet-hub

.PHONY: quickstart
quickstart: ## The whole stack in docker on http://localhost:8000: built-in login, dry run, the example registry
	scripts/quickstart

.PHONY: quickstart-down
quickstart-down: ## Stop the quickstart stack and the bots it started; data stays in deploy/compose/data
	docker compose --project-directory deploy/compose down
	docker ps -aq --filter label=wickfleet.managed=hub | xargs -r docker rm -f

##@ Image

DOCKERFILE ?= Dockerfile

.PHONY: image
image: ## Build the container image (needs docker)
	docker build --network=host -f $(DOCKERFILE) --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

##@ Website

SITE_URL     ?= https://wickfleet.com
INDEXNOW_KEY ?= 4faebe286898aed7a27eabcafbb8f100

.PHONY: site-check
site-check: ## SEO tags, sitemap, robots and internal links of site/ (fails on a gap)
	python3 scripts/site-check.py site --base $(SITE_URL) --skip 404.html

# Run after the site is live; DRY=1 prints the payload only.
.PHONY: indexnow
indexnow: ## Submit the live sitemap to IndexNow (Bing and others)
	scripts/indexnow.sh $(SITE_URL)/sitemap.xml $(INDEXNOW_KEY)

##@ Release

PUBLISH ?= $(HOME)/Development/brain/infra/tools/publish/publish

.PHONY: publish
publish: ## Snapshot to GitHub; dry run unless PUSH=1 (FIRST=1 for the first publish)
	@$(PUBLISH) $(if $(PUSH),--push) $(if $(FIRST),--first)

##@ Housekeeping

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin coverage.out $(PRIVATE_CLEAN)
