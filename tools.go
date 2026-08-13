//go:build tools

// Package tools pins the versions of developer tooling used by the CI gate
// (see Makefile: gofmt, go vet, staticcheck). Importing the command packages
// here records their versions in go.mod/go.sum so `go run` resolves the exact
// same binary locally and in CI — no @latest drift between machines.
package tools

import (
	_ "honnef.co/go/tools/cmd/staticcheck"
)
