BIN     := xkvm
VERSION ?= 0.1.0-dev

# The Go files the gofmt gate checks. Scoped to the project's own code —
# upstream/ (vendored reference) and testdata/ (binary fixtures) are excluded,
# so the check behaves identically locally and in CI.
GOFMT_SRC := cmd internal tools.go

.PHONY: build test vet tools-check gofmt-check lint lint-golangci install clean

build:
	go build -o bin/$(BIN) ./cmd/xkvm

test:
	go test ./...

vet:
	go vet ./...

# tools-check verifies tools.go's pinned imports stay consistent with go.mod
# via `go mod tidy -diff` (exits non-zero when go.mod/go.sum would change).
# tools.go cannot be compiled directly — it imports main packages, which the
# toolchain rejects by design — so tidy -diff is the canonical check that
# catches a broken import path or a deleted tools.go (which would silently
# unpin the tool version on the next tidy).
tools-check:
	go mod tidy -diff

# gofmt-check fails if any project Go file needs formatting.
gofmt-check:
	@out=$$(gofmt -l $(GOFMT_SRC)); \
	if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; exit 1; \
	fi

# lint is the CI gate, runnable locally with zero extra installs: gofmt +
# go vet + go.mod tool consistency + staticcheck + govulncheck. Both tools
# are version-pinned via the go.mod tool directive, so `go run` resolves the
# same binaries as CI.
lint: gofmt-check vet tool-check
	go run honnef.co/go/tools/cmd/staticcheck ./...
	go run golang.org/x/vuln/cmd/govulncheck ./...

# Optional heavier linter for contributors who have golangci-lint installed.
lint-golangci:
	@command -v golangci-lint >/dev/null 2>&1 || (echo "golangci-lint is not installed"; exit 1)
	golangci-lint run

# qa bundles the static + dynamic phases of docs/self-improve-protocol.md:
# the lint gate, the race-enabled suite, and the cross-compile matrix that
# catches the platform path/embed classes before CI does.
qa: lint
	go test -race ./...
	@echo "=== cross-compile matrix (darwin/linux/windows x arm64/amd64) ==="
	@for os in darwin linux windows; do \
	  for arch in arm64 amd64; do \
	    echo "  GOOS=$$os GOARCH=$$arch"; \
	    GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build ./... || exit 1; \
	  done; \
	done

install:
	go install ./cmd/xkvm

clean:
	rm -rf bin
