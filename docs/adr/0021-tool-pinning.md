# ADR-0021: Toolchain pinning with Go `tool` directives and a Brewfile

- Status: accepted · Decider: claude-senior · Date: 2026-11-12

## Context
`make bootstrap && make verify` must behave identically on a developer laptop and in CI (M01 acceptance).
The toolchain mixes Go programs (golangci-lint, buf, protoc plugins, sqlc, goose, oapi-codegen, kubeconform,
govulncheck, gitleaks, kind) and non-Go programs (helm, k6, act, hadolint, trivy, pre-commit, yq).
Unpinned `go install x@latest` or system package managers give every machine a different version and make
lint results non-reproducible.

## Options considered
1. **Go `tool` directives in a dedicated `tools` module (`tools/go.mod`) run via `go tool <name>`, plus a `Brewfile`
   for non-Go tools** — versions live in a `go.mod` (Dependabot/`go get -u` manage them), builds are cached by the
   Go toolchain, no `GOBIN` pollution, no network at run time after the first build.
   Downside: all tools share one module graph, so MVS can pick dependency versions a tool was never tested with.
   Observed in M01: gitleaks failed to compile until `charmbracelet/x/cellbuf` was bumped explicitly.
2. `go install pkg@version` with versions in the Makefile into `./bin` — each tool builds with its own `go.sum`
   (no MVS interference) but versions are scattered in Make variables, binaries are per-checkout and rebuilt per CI job.
3. Prebuilt binaries / GitHub release downloads with checksums — fastest, but a bespoke download script per tool and
   per OS/arch, and separate checksum maintenance.
4. A tools container image — reproducible, but slow on macOS (bind mounts, Docker-in-Docker for kind) and hides
   the toolchain from IDEs.

## Decision
Option 1, with two rules: (a) the `tools` module contains only tool directives and is excluded from lint/test;
(b) when MVS breaks a tool, pin the offending dependency explicitly in `tools/go.mod` with a comment, or move that
single tool to option 2 if no compatible version exists. Non-Go tools come from the `Brewfile`
(`brew bundle --no-upgrade`); CI uses the same versions through the official setup actions where brew is unavailable.

## Consequences
- Good: one `go.mod` to review for tool upgrades; `go tool` works from any directory of the workspace; `make bootstrap`
  is idempotent and offline after the first run.
- Bad / trade-offs we accept: first build of golangci-lint/buf from source takes a few minutes per machine and per
  CI cache miss; the tools module pulls a pre-release `google.golang.org/protobuf` required by buf (affects only tool
  builds, not shipped code).
- Verification: `make bootstrap` executed from a machine with none of the Go tools installed (M01 report), CI caches
  `~/.cache/go-build` and runs the same targets.

## Interview one-liner
Tool versions belong in version control next to the code they check, and `go tool` gives us that with no extra
infrastructure — at the price of watching MVS when tools share a module graph.
