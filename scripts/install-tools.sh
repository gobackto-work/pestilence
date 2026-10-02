#!/usr/bin/env bash
#
# Install the exact tool versions scripts/verify.sh asserts, for CI.
#
# A developer host installs these by hand (see docs/verification.md). CI must not:
# a runner is ephemeral, and "whatever go install gives today" would make the gate
# assert a version it did not install. Every version here matches a `require` in
# verify.sh.
#
# Usage: scripts/install-tools.sh [DEST]
set -euo pipefail

DEST="${1:-$HOME/.local/bin}"
mkdir -p "$DEST"

STATICCHECK_VERSION=2026.2.1
GOLANGCI_VERSION=v2.14.0
GITLEAKS_VERSION=8.30.1
HELM_VERSION=4.3.0

case "$(uname -m)" in
x86_64 | amd64) arch_go=amd64; arch_gitleaks=x64 ;;
aarch64 | arm64) arch_go=arm64; arch_gitleaks=arm64 ;;
*)
	echo "unsupported architecture: $(uname -m)" >&2
	exit 1
	;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "==> staticcheck $STATICCHECK_VERSION"
GOBIN="$DEST" go install "honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}"

echo "==> deadcode"
GOBIN="$DEST" go install golang.org/x/tools/cmd/deadcode@latest

echo "==> govulncheck"
GOBIN="$DEST" go install golang.org/x/vuln/cmd/govulncheck@latest

echo "==> golangci-lint $GOLANGCI_VERSION"
GOBIN="$DEST" go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_VERSION}"

echo "==> gitleaks $GITLEAKS_VERSION"
curl -fsSL "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_${arch_gitleaks}.tar.gz" |
	tar -xz -C "$DEST" gitleaks

echo "==> helm $HELM_VERSION"
curl -fsSL "https://get.helm.sh/helm-v${HELM_VERSION}-linux-${arch_go}.tar.gz" | tar -xz -C "$tmp"
install "$tmp/linux-${arch_go}/helm" "$DEST/helm"

echo
echo "installed into $DEST:"
for tool in staticcheck deadcode govulncheck golangci-lint gitleaks helm; do printf '  %-16s %s\n' "$tool" "$("$DEST/$tool" --version 2>&1 | head -1)"; done
