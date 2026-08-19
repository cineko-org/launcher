.PHONY: build check contract-check contract-release-check coverage desktop frontend-check install-wails lint security test workflow-check

GOLANGCI_LINT_VERSION ?= v2.12.2
GOVULNCHECK_VERSION ?= v1.6.0
ACTIONLINT_VERSION ?= v1.7.10
GO_FILES := $(shell find . -path './vendor' -prune -o -path './frontend' -prune -o -name '*.go' -type f -print)
NPM ?= npx --yes npm@12.0.2
WAILS ?= $(shell go env GOPATH)/bin/wails
VERSION ?= $(shell cat VERSION)

build:
	$(NPM) --prefix frontend run build
	mkdir -p bin
	go build -mod=vendor -trimpath -ldflags "-s -w" -o bin/cineko-launcher .

install-wails:
	@test -x "$(WAILS)" && "$(WAILS)" version | grep -q 'v2.14.0' || \
		go install github.com/wailsapp/wails/v2/cmd/wails@v2.14.0

desktop: install-wails
	$(WAILS) build -clean -trimpath -m -nosyncgomod \
		-ldflags "-s -w -X main.launcherVersion=$(VERSION) -X main.launcherCentralURL=$${CENTRAL_URL:-}"

lint:
	@test -z "$$(gofmt -l $(GO_FILES))" || (gofmt -l $(GO_FILES) && exit 1)
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

security:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	$(NPM) --prefix frontend audit --audit-level=moderate

coverage:
	bash scripts/unit-coverage.sh

test:
	go test -mod=vendor -race ./...

contract-check:
	grep -Eq '^# github.com/cineko-org/contracts/v3 v3.2.1( => ../contracts)?$$' vendor/modules.txt

contract-release-check:
	@! grep -Eq '^[[:space:]]*replace([[:space:]]|\()' go.mod
	@grep -Eq '^[[:space:]]*github.com/cineko-org/contracts/v3 v3.2.1$$' go.mod
	@grep -Eq '^# github.com/cineko-org/contracts/v3 v3.2.1$$' vendor/modules.txt
	@grep -Eq '^github.com/cineko-org/contracts/v3 v3.2.1 h1:' go.sum

workflow-check:
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) .github/workflows/*.yml

frontend-check:
	$(NPM) --prefix frontend run check

check: lint security coverage test frontend-check contract-check workflow-check
