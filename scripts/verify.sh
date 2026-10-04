#!/usr/bin/env bash
#
# The verification gate for pestilence. One command, identical for every agent and
# (eventually) for CI.
#
# Mirrors scarab's scripts/verify.sh deliberately: the same design rules and the same
# lint thresholds, so "verified" means the same thing on both sides of the
# contract.
#
# Design rules, each learned the hard way:
#
#   * STATELESS. Every check re-reads the tree. Nothing here is an editor, an LSP,
#     or a daemon, so nothing can serve a stale answer -- including across an agent
#     boundary, which is the case that matters when two agents share a tree.
#   * FAST. If it is not quick enough to run before every claim of "it works", it
#     will not be run. Slow checks are behind --deep.
#   * VERSION-ASSERTED. With no CI, agents install tools ad hoc. A version mismatch
#     makes "it passed for me" meaningless, so a mismatch is a FAILURE here, not a
#     warning.
#   * EXIT-CODE DRIVEN, with a quotable summary line.
#
# Usage:
#   scripts/verify.sh            # the gate
#   scripts/verify.sh --deep     # adds the slow checks
#
# Tools and the versions this was derived with:
#
#   go install honnef.co/go/tools/cmd/staticcheck@2026.2.1
#   go install golang.org/x/tools/cmd/deadcode@latest
#   go install golang.org/x/vuln/cmd/govulncheck@latest
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
#   plus the binary: gitleaks 8.30.1
#
# Not here, deliberately: no eslint/knip/jscpd (no JavaScript).
#
# A check that is skipped for one package is not recorded here. It goes in
# docs/skipped-checks.md, which names the change that removes it. A gap that nobody
# writes down becomes permanent.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

export PATH="$HOME/.local/go/bin:$HOME/go/bin:$HOME/.local/bin:$PATH"

DEEP=0
[ "${1:-}" = "--deep" ] && DEEP=1

FAILED=0
PASSED=0
OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok() { PASSED=$((PASSED + 1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() {
	FAILED=$((FAILED + 1))
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	if [ -s "$OUT" ]; then sed 's/^/       /' "$OUT" | head -25; fi
}

# check NAME CMD... — passes when CMD exits zero AND prints nothing.
check_quiet() {
	local name="$1"
	shift
	if "$@" >"$OUT" 2>&1 && [ ! -s "$OUT" ]; then ok "$name"; else bad "$name"; fi
}

# check NAME CMD... — passes when CMD exits zero, output ignored.
check() {
	local name="$1"
	shift
	if "$@" >"$OUT" 2>&1; then ok "$name"; else bad "$name"; fi
}

# require TOOL GOT WANT — a missing or mismatched tool is a failure, because
# otherwise the gate is not the same gate on both sides.
require() {
	local tool="$1" got="$2" want="$3"
	if ! command -v "$tool" >/dev/null 2>&1; then
		bad "$tool not installed (want $want)"
		return 1
	fi
	case "$got" in
	*"$want"*) return 0 ;;
	*)
		bad "$tool is $got, want $want"
		return 1
		;;
	esac
}

step "tool versions"
require go "$(go version 2>&1)" "go1.27" || true
require staticcheck "$(staticcheck -version 2>&1)" "2026.2.1" || true
require golangci-lint "$(golangci-lint --version 2>&1)" "2.14.0" || true
require govulncheck "$(govulncheck -version 2>&1 | head -1)" "go1.27" || true
require gitleaks "$(gitleaks version 2>&1)" "8.30.1" || true
require shellcheck "$(shellcheck --version 2>&1 | sed -n 's/^version: //p')" "0.11.0" || true

step "tool installer"
# Every tool the gate REQUIRES must also be installed by scripts/install-tools.sh. A
# tool the installer omits falls back to whatever the runner image happens to ship --
# which is how shellcheck 0.9.0 (ubuntu-24.04) shadowed the required 0.11.0: green on
# a developer host, red in CI. The gate is the same everywhere, or it is not a gate.
missing=""
while read -r tool; do
	case "$tool" in
	go | node | gofmt) continue ;; # from the toolchain, not the installer
	esac
	grep -q -- "$tool" scripts/install-tools.sh || missing="$missing $tool"
done < <(sed -n 's/^require \([A-Za-z][A-Za-z0-9_-]*\) .*/\1/p' scripts/verify.sh | sort -u)
if [ -n "$missing" ]; then
	bad "scripts/install-tools.sh does not install:$missing"
else
	ok "scripts/install-tools.sh covers every required tool"
fi

step "go: format, vet, lint, dead code"
check_quiet "gofmt" gofmt -l .
check "go vet" go vet ./...
check "staticcheck" staticcheck ./...
check_quiet "deadcode (unreachable functions)" deadcode ./...
check "golangci-lint (complexity, duplication, security)" golangci-lint run ./...

step "go: tests and vulnerabilities"
check "go test" go test ./...
# Race detection needs cgo. This repository IS concurrent -- the controller loop,
# the HTTP server and the store run together -- so a skip here is a real gap, not
# a formality. It needs a C toolchain: gcc on the Linux host.
if [ "$(go env CGO_ENABLED)" = "1" ]; then
	check "go test -race" go test -race ./...
else
	printf '  \033[33mskip\033[0m go test -race (CGO_ENABLED=0; needs gcc -- run on the Linux host)\n'
fi
check "govulncheck" govulncheck ./...

step "scripts and dockerfile"
check "shellcheck (scripts and setup scripts)" shellcheck -s bash scripts/*.sh cluster-setup-scripts/*.sh
check "hadolint (Dockerfile)" hadolint image/control-plane/Dockerfile

step "secrets"
# The single most valuable check in this repository. pestilence mints each
# workspace's capability token and holds the Ed25519 signing key that the broker
# verifies against -- the private key never leaves the control plane, so a leak
# here is the worst one available. Scans the tree AND the git history.
check "gitleaks (tree + git history)" gitleaks detect --source . --no-banner --redact

if [ "$DEEP" = 1 ]; then
	step "deep: test strength (slow, and it measures the SUITE, not this change)"
	if command -v gremlins >/dev/null 2>&1; then
		check "gremlins (tenant bundle)" gremlins unleash ./internal/tenant
	else
		printf '  \033[33mskip\033[0m gremlins not installed\n'
	fi
fi

printf '\n\033[1m%s: %d passed, %d failed\033[0m\n' "$(basename "$0")" "$PASSED" "$FAILED"
if [ "$FAILED" -ne 0 ]; then
	printf 'not verified\n'
	exit 1
fi
printf 'verified: %d checks, 0 failures\n' "$PASSED"
