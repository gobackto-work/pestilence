# Skipped checks

The gate in `scripts/verify.sh` is the quality bar for this repository. This file
records every check that the gate skips.

A skip is a gap. A gap that nobody writes down becomes permanent, so every skip gets
an entry here and the entry names the change that removes it.

Read this file before you review a change.

## Open skips

### `deadcode` over `internal/eventlog`

| | |
|---|---|
| Check | `deadcode (unreachable functions)` |
| Skipped for | `internal/eventlog` |
| Why | `deadcode` roots its analysis at `main`. Nothing calls this package from `main` yet, so it reports every function in it. The ingest endpoint and the delivery loop are the next change, and every function is exercised by the package's tests meanwhile. |
| Removed by | wiring the package into `cmd/control-plane`, in the same change that adds the ingest endpoint and the delivery loop |
| Risk while open | a function reachable only from a test, inside this package, is not reported as dead code |

**At review time, run the check over the whole module.** This reports nothing when the
package is healthy, and it does not skip anything:

```sh
go list ./... | xargs deadcode
```

## What a new skip needs

An entry here must state four things, because a skip without them cannot be removed:

1. the check that is skipped,
2. what it is skipped for,
3. why it cannot pass now,
4. the change that removes it.

Add the entry in the same change that adds the skip. Remove the entry in the same
change that removes the skip.

## Gaps that are not skips

The comments at the top of `scripts/verify.sh` record the checks that do not exist
at all. Those are not skips, because no check reports success while the gap is open.
Read those comments, and not this file, for that list.
