# Skipped checks

The gate in `scripts/verify.sh` is the quality bar for this repository. This file
records every check that the gate skips.

A skip is a gap. A gap that nobody writes down becomes permanent, so every skip gets
an entry here and the entry names the change that removes it.

Read this file before you review a change.

## Open skips

None. The gate checks everything it says it checks.

`internal/eventlog` was skipped here between the change that added it and the change
that wired the last of it in. The entry is gone with the wiring.

**Worth knowing about the check.** `deadcode` reports a method as reachable when its
type satisfies an interface that something reachable uses, and it cannot tell which of
those methods is actually called. So a type held behind an interface goes quiet in this
check even if most of its methods have no caller. It is a real limit, and it is why the
planted-function check above is worth running by hand.

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
