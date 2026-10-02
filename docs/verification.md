# Verification

The gate is `hack/verify.sh`. One command, identical for every agent and
(eventually) for CI:

```bash
bash hack/verify.sh          # the gate — seconds
bash hack/verify.sh --deep   # adds the slow checks
```

It prints a per-check result and ends with a quotable line:

```
verify.sh: 8 passed, 0 failed
verified: 8 checks, 0 failures
```

Mirrors `scarab/hack/verify.sh` deliberately: the same design rules and the same
lint thresholds, so "verified" means the same thing on both sides of the contract.

## The four design rules

1. **Stateless.** Every check re-reads the tree. Nothing in the gate is an editor,
   an LSP, or a daemon, so nothing can serve a stale answer — including across an
   agent boundary, which is the case that matters when two agents share a tree.
2. **Fast enough to actually run.** A gate an agent skips is worse than no gate.
   Anything slow goes behind `--deep`.
3. **Version-asserted.** With no CI, agents install tools ad hoc, and a version
   mismatch makes "it passed for me" meaningless. A mismatch is a **failure**, not
   a warning.
4. **Exit-code driven**, with a summary line worth pasting.

## What the gate runs

| Check | Version | Catches | Install |
|---|---|---|---|
| `gofmt -l` | go1.27.1 | formatting | toolchain |
| `go vet` | go1.27.1 | standard analyzers | toolchain |
| `staticcheck` | 2026.2.1 | correctness, simplifications, unused | `go install honnef.co/go/tools/cmd/staticcheck@2026.2.1` |
| `deadcode` | x/tools | functions unreachable from a binary | `go install golang.org/x/tools/cmd/deadcode@latest` |
| `golangci-lint` | v2.14.0 | **complexity** (`gocognit`, `gocyclo`, `funlen`, `nestif`, `maintidx`), **duplication within the module** (`dupl`), `revive`, `gocritic`, `misspell`, `unparam`, `gosec` | `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0` |
| `go test` | — | behaviour | toolchain |
| `go test -race` | — | data races | **Linux host** (needs cgo) |
| `govulncheck` | — | known vulnerabilities in dependencies | `go install golang.org/x/vuln/cmd/govulncheck@latest` |
| `gitleaks` | 8.30.1 | secrets in the tree **and the git history** | release binary |

## The check that matters most here

**`gitleaks`.** pestilence mints every workspace's capability token and holds the
Ed25519 private key the broker verifies against. The private key never leaves the
control plane, so a leak here is the worst one available — worse than a leaked
model key, because it lets the holder forge a capability token for any workspace.

`gitleaks detect` scans the git history as well as the tree, which is deliberate:
removing a secret in a later commit does not un-leak it.

## Not here, and why

| Missing | Reason |
|---|---|
| `eslint`, `knip`, `jscpd` | no JavaScript |
| `hadolint` | a Dockerfile exists (`image/control-plane/`) but the check is not wired in yet |
| `shellcheck` | `hack/*.sh` exist but the check is not wired in yet |

Each is a one-line addition when the artifact appears. Leaving them out now keeps
the gate honest about what it actually covers.

## Running the gate on the Linux host

`go test -race` needs cgo, so it cannot run on Windows and **skips visibly** there.
That is not a formality: the controller loop, the HTTP server and the store run
concurrently. So the gate has two homes:

| Host | Checks | `-race` |
|---|---|---|
| Windows (authoring) | 8 | skipped |
| Linux host | **9** | **runs** |

Install the tools on the Linux host — all pure Go, so no root needed:

```bash
go install honnef.co/go/tools/cmd/staticcheck@2026.2.1
go install golang.org/x/tools/cmd/deadcode@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
```

**gitleaks is the exception: use the release binary, not `go install`.** Two
reasons: at 8.30.1 the module path is still
`github.com/zricethezav/gitleaks/v8`, so the current path fails to resolve; and a
`go install` build reports `version is set by build process`, which the version
assertion rejects — correctly, because a gate that cannot state its own version is
not the same gate.

```bash
VER=8.30.1
curl -sSL -o /tmp/gl.tgz \
  "https://github.com/gitleaks/gitleaks/releases/download/v${VER}/gitleaks_${VER}_linux_x64.tar.gz"
mkdir -p ~/.local/bin && tar -xzf /tmp/gl.tgz -C ~/.local/bin gitleaks
```

Verified on the Linux host: `verify.sh: 9 passed, 0 failed`.

## Thresholds are hard limits

`.golangci.yml` is deliberately identical to scarab's, so a limit is not a
per-repository opinion. For an agent a complexity limit is a forcing function — it
makes the code get restructured rather than annotated. The first run of this gate
produced ten findings and every one was acted on rather than suppressed.

Two exclusions exist and both are justified in the config: `gosec`'s `G101`
("hardcoded credentials") fires on the *names* a Secret is addressed by, not on a
key; and one `nolint:gosec` in `render-bundle` covers an integer conversion that
`G115` cannot see is range-checked one line above. A `.gitleaks.toml` allowlist
covers a historical PEM *header* literal in a test — a header string is not a key,
and rewriting history to satisfy a linter would be worse than excluding the hit.

**Do not add an exclusion without reading the hit first.**

## Necessary, not sufficient

A gate is a floor. The bugs that actually took this platform down were found by
provisioning a real workspace and watching the API server disagree — a mount path
`InClusterConfig` could not read, a Secret mode a non-root uid could not open, a
`nodeName` rule that blocked the Job controller from cleaning up. None of those is
visible to a linter.

So: run the gate before claiming anything works, and still provision a real
workspace as the final word. The live check found more than every linter combined.
