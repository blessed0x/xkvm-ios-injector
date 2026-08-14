# Contributing to xkvm

Thanks for wanting to help! This guide is short: build it, test it, and pin your changes.

## Getting started

**Requirements:** Go 1.26+, macOS for the native-toolchain tests.

```bash
make build    # produces ./bin/xkvm
make test     # full test suite
make lint     # gofmt + go vet + staticcheck — the CI gate
```

## The house rules

1. **Every behavior change ships with a test.** This project's whole point is
   fidelity to upstream tools, so the house style is *golden tests*: run the
   real upstream tool, capture its output, and pin xkvm's output byte-for-byte
   against it (see `internal/rootless/script_golden_test.go` for the pattern).
   Prove changes red-before-green: make the test fail first, then fix.

2. **Run the full gate before pushing.** `make lint` plus
   `go test -race ./...`. Some tests are native-toolchain-gated and only run on
   macOS — they are skipped elsewhere, so a green Linux run alone is not proof.

3. **Stay in scope.** If you notice an unrelated bug, open an issue instead of
   folding the fix into your PR.

4. **Keep the docs honest.** `ARCHITECTURE.md` documents every deviation from
   upstream. If you change a behavior, update the doc that claims it.

## Submitting changes

1. Fork the repo and create a branch: `git checkout -b fix/whatever`.
2. Make your change with its test.
3. Run the gate: `make lint && go test -race ./...`.
4. Open a pull request. Describe what changed and **why** — the why matters
   more than the what.
5. CI runs the same gate on macOS and Linux; wait for it to go green.

## Asking questions

For feature ideas and design questions, open an issue *before* writing code.
The project is small and deliberately scoped — see the README's
[Project status](README.md#project-status) for what is planned and what is not.
