package eventlog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// The tests below are the invariants in docs/event-record.md. Each test names the
// invariant it holds, so that a change to the behaviour has to change a named rule.

func open(t *testing.T) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func newID() string { return ulid.Make().String() }

// fixture is one workspace, one run and one owner.
type fixture struct {
	log         *Log
	runID       string
	workspaceID string
	ownerID     string
}

func newFixture(t *testing.T) fixture {
	return fixture{log: open(t), runID: newID(), workspaceID: newID(), ownerID: newID()}
}

func (f fixture) report(state State) Report {
	return Report{
		ID: newID(), RunID: f.runID, WorkspaceID: f.workspaceID, OwnerID: f.ownerID,
		State: state, Mode: ModeInteractive, OccurredAt: time.Now().UTC(),
	}
}

func (f fixture) append(t *testing.T, states ...State) {
	t.Helper()
	for _, s := range states {
		if _, err := f.log.Append(context.Background(), f.report(s)); err != nil {
			t.Fatalf("append %s: %v", s, err)
		}
	}
}

func (f fixture) events(t *testing.T) []Event {
	t.Helper()
	events, err := f.log.EventsAfter(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}

// Invariant 1: a repeated event id produces one row.
func TestARepeatedReportAddsOneEvent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	report := f.report(StateRunning)

	first, err := f.log.Append(ctx, report)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !first.Appended {
		t.Fatal("the first report appended nothing")
	}

	// A retry of the same report is what a broker sends after a timeout.
	retry, err := f.log.Append(ctx, report)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.Appended {
		t.Error("the retry appended a second event")
	}
	if retry.Sequence != first.Sequence {
		t.Errorf("retry sequence = %d, want %d", retry.Sequence, first.Sequence)
	}
	if got := f.events(t); len(got) != 1 {
		t.Errorf("got %d events, want 1", len(got))
	}
}

// Invariant 2: sequence never decreases and is unique.
//
// The interesting case is a delete, because a bare rowid is reused after the highest
// row is removed. A reused sequence would sit at or below a subscriber's cursor, so
// the subscriber would discard a new event as one it had already seen.
func TestASequenceIsNeverReusedAfterADelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.append(t, StateRunning, StateWaiting)

	before := f.events(t)
	highest := before[len(before)-1].Sequence

	if err := f.log.DeleteWorkspace(ctx, f.workspaceID); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	f.append(t, StateRunning)

	after := f.events(t)
	if len(after) != 1 {
		t.Fatalf("got %d events after the delete, want 1", len(after))
	}
	if after[0].Sequence <= highest {
		t.Errorf("sequence %d was reused at or below %d, so a subscriber would discard it",
			after[0].Sequence, highest)
	}
}

// Invariant 3, and the derivation: the record names the event, not the runtime.
func TestTheRecordDerivesTheKindFromTheStates(t *testing.T) {
	f := newFixture(t)
	f.append(t, StateRunning, StateWaiting, StateRunning, StateSucceeded)

	got := f.events(t)
	want := []Kind{KindRunStarted, KindRunWaiting, KindRunResumed, KindRunSucceeded}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, kind := range want {
		if got[i].Kind != kind {
			t.Errorf("event %d kind = %s, want %s", i, got[i].Kind, kind)
		}
	}
	last := got[len(got)-1]
	if last.PreviousState != StateRunning || last.State != StateSucceeded {
		t.Errorf("last event = %s to %s, want running to succeeded", last.PreviousState, last.State)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Sequence <= got[i-1].Sequence {
			t.Errorf("event %d sequence %d does not follow %d", i, got[i].Sequence, got[i-1].Sequence)
		}
	}
}

func TestAnUnreachableStateIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.append(t, StateRunning, StateSucceeded)

	// A run that has ended cannot start again. A second run gets a second id.
	if _, err := f.log.Append(ctx, f.report(StateRunning)); !errors.Is(err, ErrTransition) {
		t.Fatalf("append after a terminal state = %v, want ErrTransition", err)
	}
}

// The re-assertion case. A runtime that reports the state the record already holds is
// not making an error, and it must read the answer as success.
func TestReAssertingTheCurrentStateIsNotAnError(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first, err := f.log.Append(ctx, f.report(StateRunning))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	again, err := f.log.Append(ctx, f.report(StateRunning))
	if err != nil {
		t.Fatalf("re-assert: %v", err)
	}
	if again.Appended {
		t.Error("the re-assertion appended an event")
	}
	if again.Sequence != first.Sequence {
		t.Errorf("re-assert sequence = %d, want %d", again.Sequence, first.Sequence)
	}
}

// Invariant 5: an event is not delivered to a subscription that is not entitled to it.
func TestAnExternalSubscriptionNeverSeesAnotherPrincipal(t *testing.T) {
	l := open(t)
	ctx := context.Background()

	mine, theirs, workspace := newID(), newID(), newID()
	appendFor := func(owner string) {
		t.Helper()
		report := Report{ID: newID(), RunID: newID(), WorkspaceID: workspace,
			OwnerID: owner, State: StateRunning, Mode: ModeInteractive, OccurredAt: time.Now().UTC()}
		if _, err := l.Append(ctx, report); err != nil {
			t.Fatalf("append for %s: %v", owner, err)
		}
	}
	appendFor(mine)
	appendFor(theirs)

	sub := Subscription{ID: newID(), Kind: KindPush, PrincipalID: mine,
		URL: "https://hooks.example.com/events", Secret: "not-a-real-secret"}
	if err := l.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	_, events, err := l.Outstanding(ctx, sub.ID, 10)
	if err != nil {
		t.Fatalf("outstanding: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].OwnerID != mine {
		t.Errorf("an external subscription received owner %s, want %s", events[0].OwnerID, mine)
	}
}

func TestAPullSubscriptionTakesOnlyItsPrincipalsEvents(t *testing.T) {
	l := open(t)
	ctx := context.Background()

	workspace, mine := newID(), newID()
	for _, owner := range []string{mine, newID()} {
		report := Report{ID: newID(), RunID: newID(), WorkspaceID: workspace,
			OwnerID: owner, State: StateRunning, Mode: ModeInteractive, OccurredAt: time.Now().UTC()}
		if _, err := l.Append(ctx, report); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	sub, err := l.EnsurePull(ctx, mine)
	if err != nil {
		t.Fatalf("ensure pull: %v", err)
	}
	if sub.Kind != KindPull {
		t.Errorf("kind = %s, want %s", sub.Kind, KindPull)
	}

	// A second call returns the same subscription. The unique constraint on the principal
	// and the kind is what makes that true.
	again, err := l.EnsurePull(ctx, mine)
	if err != nil {
		t.Fatalf("ensure pull twice: %v", err)
	}
	if again.ID != sub.ID {
		t.Errorf("the second call created a second subscription: %s then %s", sub.ID, again.ID)
	}

	_, events, err := l.Outstanding(ctx, sub.ID, 10)
	if err != nil {
		t.Fatalf("outstanding: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1, so a subscription saw another principal's run", len(events))
	}
	if events[0].OwnerID != mine {
		t.Errorf("the subscription received owner %s, want %s", events[0].OwnerID, mine)
	}
}

// A pull subscription belongs to a principal, so nothing may ask for one through the path
// that creates push ones: it would be a subscription for someone else's events.
func TestCreateSubscriptionRefusesAPullKind(t *testing.T) {
	l := open(t)
	err := l.CreateSubscription(context.Background(), Subscription{
		ID: newID(), Kind: KindPull, PrincipalID: newID(),
	})
	if !errors.Is(err, ErrPullSubscription) {
		t.Fatalf("CreateSubscription(pull) = %v, want ErrPullSubscription", err)
	}
}

// Invariant 6: an attributes map contains no value that could carry content.
//
// There is no free-text key, so the rule is enforced by the schema and not by
// inspection.
func TestAttributesRejectAnythingThatCouldCarryContent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	refused := map[string]map[string]any{
		"an unknown key":        {"prompt": "summarise the repository"},
		"a numeric key as text": {"tokens": "the whole conversation"},
		"ident with spaces":     {"model": "please read my private notes"},
		"ident that is long":    {"model": string(make([]byte, 200))},
		"a fractional count":    {"tokens": 1.5},
		"a nested object":       {"tokens": map[string]any{"a": 1}},
	}
	for name, attributes := range refused {
		t.Run(name, func(t *testing.T) {
			report := f.report(StateRunning)
			report.Attributes = attributes
			if _, err := f.log.Append(ctx, report); !errors.Is(err, ErrInvalidAttribute) {
				t.Errorf("append = %v, want ErrInvalidAttribute", err)
			}
		})
	}

	accepted := f.report(StateRunning)
	accepted.Attributes = map[string]any{
		"tokens": 120, "duration_ms": int64(3000), "tools": 4, "model": "claude-sonnet-4",
	}
	if _, err := f.log.Append(ctx, accepted); err != nil {
		t.Fatalf("a valid payload was refused: %v", err)
	}
	got := f.events(t)
	if len(got) != 1 || got[0].Attributes["model"] != "claude-sonnet-4" {
		t.Errorf("attributes did not survive the round trip: %+v", got)
	}
}

// Invariant 7: deleting a workspace removes its runs and events.
func TestDeletingAWorkspaceRemovesItsRunsAndEvents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.append(t, StateRunning, StateWaiting)

	if err := f.log.DeleteWorkspace(ctx, f.workspaceID); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	if got := f.events(t); len(got) != 0 {
		t.Errorf("got %d events after the delete, want 0", len(got))
	}
	if _, err := f.log.Run(ctx, f.runID); !errors.Is(err, ErrNotFound) {
		t.Errorf("run after the delete = %v, want ErrNotFound", err)
	}
}

func TestPruneKeepsTheNewestEvents(t *testing.T) {
	f := newFixture(t)
	f.log.maxEvents = 3
	ctx := context.Background()

	f.append(t, StateRunning)
	for range 5 {
		f.append(t, StateWaiting, StateRunning)
	}
	all := f.events(t)
	if len(all) != 11 {
		t.Fatalf("got %d events, want 11", len(all))
	}

	result, err := f.log.Prune(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.ByAge != 0 {
		t.Errorf("pruned %d by age, want none", result.ByAge)
	}
	if result.ByCount != 8 {
		t.Errorf("pruned %d by count, want 8", result.ByCount)
	}

	left := f.events(t)
	if len(left) != 3 {
		t.Fatalf("kept %d events, want 3", len(left))
	}
	if left[0].Sequence != all[8].Sequence {
		t.Errorf("kept from sequence %d, want %d", left[0].Sequence, all[8].Sequence)
	}
}

// The rule that makes a small size limit safe: the limit applies to delivered events,
// so it can never discard one that nobody has seen.
func TestPruneNeverDiscardsWhatASubscriptionHasNotTaken(t *testing.T) {
	f := newFixture(t)
	f.log.maxEvents = 2
	ctx := context.Background()
	f.append(t, StateRunning, StateWaiting, StateRunning)

	sub := Subscription{ID: newID(), Kind: KindPush, PrincipalID: f.ownerID,
		URL: "https://hooks.example.com/events", Secret: "not-a-real-secret"}
	if err := f.log.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	result, err := f.log.Prune(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.ByAge != 0 || result.ByCount != 0 {
		t.Errorf("discarded %d by age and %d by count, want none", result.ByAge, result.ByCount)
	}
	if result.Held != 3 {
		t.Errorf("Held = %d, want 3, so that a stuck subscription is visible", result.Held)
	}

	_, events, err := f.log.Outstanding(ctx, sub.ID, 10)
	if err != nil {
		t.Fatalf("outstanding: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("the subscription has %d events, want 3", len(events))
	}

	// Once the subscription has taken them, the size limit applies again.
	if err := f.log.AdvanceCursor(ctx, sub.ID, events[2].Sequence); err != nil {
		t.Fatalf("advance cursor: %v", err)
	}
	result, err = f.log.Prune(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if result.ByCount != 1 {
		t.Errorf("pruned %d by count once the cursor moved, want 1", result.ByCount)
	}
}

func TestAFailedSubscriptionStopsHoldingTheRecord(t *testing.T) {
	f := newFixture(t)
	f.log.maxEvents = 1
	ctx := context.Background()
	f.append(t, StateRunning, StateWaiting)

	sub := Subscription{ID: newID(), Kind: KindPush, PrincipalID: f.ownerID,
		URL: "https://hooks.example.com/events", Secret: "not-a-real-secret"}
	if err := f.log.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if err := f.log.Fail(ctx, sub.ID); err != nil {
		t.Fatalf("fail: %v", err)
	}

	result, err := f.log.Prune(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.ByCount != 1 {
		t.Errorf("pruned %d by count, want 1. A failed subscription must not hold the record for ever", result.ByCount)
	}
}

func TestTheRunSummaryFollowsTheLog(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.append(t, StateRunning, StateWaiting, StateSucceeded)

	run, err := f.log.Run(ctx, f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != StateSucceeded {
		t.Errorf("state = %s, want succeeded", run.State)
	}
	if run.EndedAt == nil {
		t.Error("a terminal run has no ended_at")
	}
	if run.LastSequence == 0 {
		t.Error("last_sequence is not set")
	}
	if run.WorkspaceID != f.workspaceID || run.OwnerID != f.ownerID {
		t.Errorf("the summary lost its workspace or owner: %+v", run)
	}
}

// The two pragmas are per-connection settings, so they live in the DSN. A mistake
// there is silent, and this is the test that catches it.
func TestTheFileUsesWALAndWaitsOnALock(t *testing.T) {
	l := open(t)
	ctx := context.Background()

	var mode string
	if err := l.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	var timeout int
	if err := l.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

// The empty payload must round trip as an empty map and not as a null.
func TestAnEmptyPayloadRoundTrips(t *testing.T) {
	f := newFixture(t)
	f.append(t, StateRunning)

	got := f.events(t)
	if got[0].Attributes == nil {
		t.Fatal("attributes came back nil")
	}
	if len(got[0].Attributes) != 0 {
		t.Errorf("attributes = %+v, want empty", got[0].Attributes)
	}
}

// A batch run has nobody to answer it, so it cannot reach a state that waits for a
// person. Without this rule a parent waiting on a worker would wait for ever.
func TestABatchRunCannotWaitForAPerson(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	start := f.report(StateRunning)
	start.Mode = ModeBatch
	if _, err := f.log.Append(ctx, start); err != nil {
		t.Fatalf("a batch run cannot start: %v", err)
	}

	waiting := f.report(StateWaiting)
	waiting.Mode = ModeBatch
	if _, err := f.log.Append(ctx, waiting); !errors.Is(err, ErrTransition) {
		t.Fatalf("a batch run reached waiting: %v, want ErrTransition", err)
	}
}

// The mode decides the state machine, so the first report fixes it.
func TestTheModeOfARunCannotChange(t *testing.T) {
	f := newFixture(t)
	f.append(t, StateRunning)

	changed := f.report(StateWaiting)
	changed.Mode = ModeBatch
	if _, err := f.log.Append(context.Background(), changed); !errors.Is(err, ErrModeChanged) {
		t.Fatalf("a mode change = %v, want ErrModeChanged", err)
	}
}

func TestAnUnknownModeIsRefused(t *testing.T) {
	f := newFixture(t)
	report := f.report(StateRunning)
	report.Mode = ""
	if _, err := f.log.Append(context.Background(), report); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an empty mode = %v, want ErrInvalid", err)
	}
}

func TestTheRunSummaryKeepsTheMode(t *testing.T) {
	f := newFixture(t)
	f.append(t, StateRunning)

	run, err := f.log.Run(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Mode != ModeInteractive {
		t.Errorf("mode = %q, want %q", run.Mode, ModeInteractive)
	}
}
