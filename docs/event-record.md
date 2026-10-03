# The event record

This document defines the durable record of run events, and the delivery of those events
to sinks. It defines the contract between the runtime, the control plane, and the
delivery sinks.

## Purpose

A notification must name the thing that happened. A subscriber must resume after a
restart without losing an event or repeating one. Both need one record with an identity
and an order.

Three features read the record:

| Feature | What it reads the record for |
|---|---|
| Push notifications | to notify once per milestone |
| MCP events | to deliver a subscription backlog after a reconnect |
| Run-until-complete | to resume a parent run when a child run ends |

## Terms

| Term | Meaning |
|---|---|
| Run | One agent execution in one workspace, from its start to a terminal state. |
| Event | One recorded state transition of one run. |
| Sequence | An integer that orders events in the record. It is unique and it never decreases. |
| Subscription | A durable request for events, held by one principal. |
| Sink | A destination for a delivered event. The sinks are SNS and an MCP callback URL. |
| Cursor | The highest sequence a subscription has acknowledged. |

## Ownership

The control plane (pestilence) owns the record. It is the only writer.

| Party | Responsibility |
|---|---|
| The runtime (scarab) | Decides a run's state. Reports each transition. |
| The control plane (pestilence) | Assigns the sequence, records the event, and delivers it. |
| town | Holds the users and the devices. Receives notifications for delivery to SNS. |
| The external integration | Holds a subscription. Receives events at a callback URL. |

The runtime decides state. The control plane records it. The control plane does not
infer a state, and it does not retry a report that the runtime did not make.

## The event

An event is immutable after it is written. It has these fields:

| Field | Type | Meaning |
|---|---|---|
| `sequence` | integer | Assigned by the control plane on append. Unique, monotonic. |
| `event_id` | string | Supplied by the runtime. Unique for one event. |
| `run_id` | string | The run the event belongs to. |
| `workspace_id` | string | The workspace that owns the run. |
| `owner_id` | string | The principal that owns the workspace. |
| `kind` | string | The transition, from the closed set below. |
| `previous_state` | string | The run state before the transition. |
| `state` | string | The run state after the transition. |
| `occurred_at` | timestamp | When the runtime observed the transition. |
| `recorded_at` | timestamp | When the control plane appended the event. |
| `attributes` | map | Small, non-sensitive values. See the payload rule below. |

### The payload rule

The `attributes` map must be safe to hand to a third party. It holds counts, durations
and identifiers. It does not hold a prompt, a completion, a transcript line, a file path,
a tool argument, an error string, or a credential.

This rule is the privacy boundary for the MCP integration, and it applies to every sink
alike. A separate rule for one sink will be forgotten.

The map is capped at 1 KB. The retention limit counts events, so one large map would break
the equivalence between the count and the size.

### `attributes` is a payload and never a predicate

Nothing selects an event by a value inside `attributes`. Every read uses `sequence`,
`run_id`, `workspace_id`, `state` or `kind`, and each of those is a column.

This is what makes a JSON column in a relational database the right choice. The access
pattern is an append and then a scan forward from a cursor, which is what a rowid table
does best. A document store would add query freedom that nothing needs, and give up the
transactional ordered append that everything depends on.

The rule that keeps it true: **an attribute that is ever queried becomes a column.** A
queried attribute is a first-class field wearing a disguise. SQLite can also index into
JSON with a generated column, so the escape hatch exists without changing the database:

    ALTER TABLE events ADD COLUMN tokens INTEGER
      GENERATED ALWAYS AS (json_extract(attributes, '$.tokens')) VIRTUAL;

### Encoding

| Value | Encoding |
|---|---|
| `sequence` | Integer. Assigned by the control plane, monotonic. |
| `event_id`, `run_id`, `workspace_id`, `owner_id` | ULID, as text. |
| `occurred_at`, `recorded_at` | Integer epoch milliseconds, UTC. |
| `attributes` | JSON object as text. Capped at 1 KB. |
| `kind`, `state`, `previous_state` | Text, from the closed sets. |

### The closed set of kinds

The `kind` values are the run state transitions. There is no open extension point. A new
kind needs a change to this document.

| `kind` | From | To | Notifiable |
|---|---|---|---|
| `run.started` | — | `running` | no |
| `run.waiting` | `running` | `waiting` | yes |
| `run.resumed` | `waiting` | `running` | no |
| `run.succeeded` | `running` | `succeeded` | yes |
| `run.failed` | `running` | `failed` | yes |
| `run.cancelled` | `running`, `waiting` | `cancelled` | yes |
| `run.budget_exhausted` | `running` | `budget_exhausted` | yes |

The Notifiable column is the closed vocabulary for features 3 and 4. A `run.resumed`
event is recorded so that a subscriber can reconstruct the state, and it is not sent to a
notification sink.

## The run state machine

| State | Terminal | Meaning |
|---|---|---|
| `running` | no | The agent is working. |
| `waiting` | no | The agent needs input from a person. |
| `succeeded` | yes | The agent reached its goal. |
| `failed` | yes | The agent stopped on an error. |
| `cancelled` | yes | A person stopped the run. |
| `budget_exhausted` | yes | The run reached a token, time or spend limit. |

`budget_exhausted` exists because run-until-complete makes a mid-run limit normal rather
than rare. A run that stops for a budget must be distinguishable from one that failed,
because the person can act on the first and not the second.

The state values are the runtime's contract. This document records them and does not
define them.

## The write path

The runtime reports a transition to the control plane over one authenticated endpoint.

1. The agent sends a state message over the workspace WebSocket. The message names the
   run and the state.
2. The broker validates the message and forwards it to the control plane as one event,
   with its `event_id`.
3. The control plane authenticates the caller. See below.
4. The control plane appends the event and assigns the `sequence`.
5. The control plane answers with the assigned `sequence`.

The agent is the only party that can tell a run that has finished from a run that is
waiting for a person. The broker observes the protocol and cannot. The broker therefore
does not infer a state, and the agent does not hold a credential for the control plane.

The broker must treat a report as successful only when it receives a `sequence`. On a
timeout the broker retries with the same `event_id`.

### A transition that the control plane does not accept

The broker drops an event that the control plane does not accept, and it does not buffer
it. A lost notification is accepted.

**A dropped event is recoverable. A stale state is not.** The broker asserts its current
state again when it next reaches the control plane. Without that, the record can stay
wrong: it shows `running` for a run that is waiting. Every reader acts on the state, so a
wrong state stops a parent run from resuming, and it hides the run from the person who is
waiting to be told about it.

The asserted transition carries a new `event_id`, and its `previous_state` is the state
that the record already held. An intermediate transition that was dropped is not
recovered.

### Idempotency of the write

`event_id` has a unique index. A repeated `event_id` does not append a second row. The
control plane answers with the sequence of the first row.

This makes the write exactly-once as observed by the control plane, while the delivery
stays at-least-once. The two different guarantees are deliberate. A duplicate delivery is
cheap to discard. A duplicate event in the record is not.

### Authentication of the write

The runtime presents the workspace capability token, which already exists. Two changes
are necessary:

- The token must carry an audience for this endpoint. A token minted for the broker must
  not be accepted here.
- The token must carry the reporting role. Do not accept a role that may orchestrate.

The control plane verifies the token against the workspace public key. It does not hold
the signing key for a tenant namespace either, and it must not start to.

## The read path

A subscription reads events with a cursor.

- The control plane delivers events in ascending `sequence` order.
- Delivery for one subscription is at-least-once.
- A subscriber discards an event whose `sequence` is not greater than its cursor.
- A subscriber stores its cursor after it processes a batch.

A subscriber that loses its cursor re-reads from an earlier position and discards
duplicates. A subscriber must therefore be idempotent on `sequence`.

Ordering is guaranteed within one run, because a run's events have ascending sequences.
Ordering between runs is not guaranteed and is not useful.

### Head-of-line behaviour

One subscription has one cursor, and the cursor does not advance past a failed delivery.
A slow sink therefore delays that subscription and no other. This is accepted. If it
becomes a problem, the cursor moves to the run, at the cost of one cursor per run.

## Subscriptions

One table holds every subscription. A `kind` column separates the two shapes.

| Column | Internal | External |
|---|---|---|
| `id` | ULID | ULID |
| `kind` | `internal` | `external` |
| `principal_id` | the control plane | the subscribing user |
| `filter` | none. Every event. | the principal's workspaces |
| `url` | absent | the callback URL |
| `secret` | absent | the signing secret |
| `cursor` | the last accepted sequence | the last acknowledged sequence |
| `state` | active | active, or failed |

### Why town is not an ordinary subscriber

The difference is not trust. It is the unit of fan-out.

An external subscription belongs to one principal. It receives that principal's events
and nothing else. town delivers for every user, so it must see every event and decide per
event, and per registered device, whether to notify.

The alternative is one subscription per user, each filtered to that user. That means a
cursor, a retry state and a failure mode per user, and a new row whenever a user signs up.
The internal subscription removes all of it. town does the fan-out, because town holds the
users and the devices.

### The invariant that protects it

**Only the control plane can create an internal subscription.** No request path may let a
principal choose `kind`. An internal subscription receives every tenant's events, so a
principal who could create one would see every other user's runs. Test this as an
authorisation failure, and not as a validation failure.

## Storage

The event record uses its own SQLite file. It does not share the file that holds the
workspace registry.

The two files have different shapes. The registry is read-mostly, and it holds a row per
workspace. The record is append-heavy, and it holds a row per transition. One file with
one writer means the two contend for one lock. Two files never contend.

## Retention

The record serves two purposes with two different lifetimes.

| Purpose | Lifetime |
|---|---|
| Delivery | Until every subscription cursor has passed the event, and a limit below is reached. |
| Run history | One summary row per run, kept until the workspace is deleted. |

An event is discarded when the first of these limits is reached:

| Limit | Value |
|---|---|
| Age | 7 days |
| Recorded events | The newest 1700 events |

**An event that an active subscription has not acknowledged is never discarded.** The
size limit applies to delivered events only. Without this rule the size limit weakens the
delivery guarantee, and a subscriber that was away for a day loses events that are still
inside the age limit.

A subscription that prevents the size limit from being met is a fault. Report it. Do not
resolve it by discarding an event that nobody has seen.

A subscription whose deliveries pass the retry limit is marked failed and stops holding a
cursor. A subscriber that is down for a long time therefore cannot hold events past their
age limit for ever, and the size limit stays reachable.

### What the size limit holds

The size limit is a backlog budget, not a history budget. The rule that an unacknowledged
event is never discarded means the limit can only remove events that every subscriber has
already seen. A small limit therefore costs no correctness. It shortens the backlog that
a subscriber may fall behind by.

0.5 MB is about 1700 events at 300 bytes each, including the indexes. A run produces one
event at its start, two for each exchange with a person, and one at its end. A run with
five exchanges produces twelve events, so 0.5 MB holds about 140 such runs of backlog.

The age limit applies in normal operation. The size limit applies when a subscriber falls
behind, or when the platform is busy. A subscription that holds the record above its size
limit is a fault, so the limit also acts as a detector.

The limit is a count of events and not a byte size. The `sequence` orders the events, so
"keep the newest 1700" is one delete. A byte budget needs a measure of the file, and that
measure drifts with the row size. 1700 events is about 0.5 MB, so the two statements agree,
and one constant is the whole policy.

SQLite does not shrink its file when rows are deleted. It reuses the pages, so the file
stays at its high-water mark. The write-ahead log needs room beside it, and the log can be
larger than the database while a writer is busy. Budget for both.

The event rows are a queue. The run summary is the log. A query for "what did this run
do" reads the summary. A query for "what did this agent do in March" reads the transcript
on the workspace volume, if the workspace still exists.

An event that a sink has not acknowledged is deleted when its workspace is deleted.
Deletion wins over delivery. A subscriber must not treat a gap as an error.

## Deletion

Deleting a workspace deletes its runs, its events and its subscriptions.

There is no history of a deleted workspace. The transcript lives on the workspace volume
and is removed with the namespace. A person who needs long-term history needs a feature
that does not exist yet.

## Delivery to a sink

Each sink has a delivery row per event, with a state of `pending`, `delivered` or
`failed`.

- The control plane retries a failed delivery with backoff.
- A delivery that exceeds the backoff limit is marked `failed` and reported as a metric.
  It is not retried again without an operator action.
- Every delivery is signed. The signature covers the payload, a timestamp and a nonce.
  A receiver can therefore verify the control plane and detect a replay.

### The SNS sink

The control plane does not call SNS. It delivers the event to town, and town calls SNS.
town holds the user and the device registration, so town owns the credentials and the
per-user policy.

The control plane must send town an idempotency key, so that a duplicate delivery does
not send two notifications.

### The MCP sink

The MCP callback is a request to a URL that a subscriber supplied. Treat that URL as
untrusted input:

- Accept HTTPS only.
- Refuse a private, loopback, link-local or cluster-internal address.
- Resolve the name, then connect to the resolved address. Do not resolve again.
- Refuse a redirect.
- Set a timeout and cap the response body.

An MCP subscription is held by one principal. The control plane must filter the events
for that subscription by that principal's entitlements at delivery time, and not only at
the time the subscription is created. An entitlement that is removed must stop the
events.

## Notification policy

A recorded event and a notification are not the same thing. The record states a fact. A
notification is a decision about that fact. The decisions belong to a policy stage
between the record and the sinks, and town owns that stage.

| Decision | Decided by | Cost of a wrong answer |
|---|---|---|
| Whether a run reached a terminal state | the runtime | a run stalls or stops early, and it cannot be recovered |
| Which event is notifiable | this document | none. The set is closed. |
| Whether to notify for one event | policy | a person is not told, and no event is lost |
| Which category a waiting run falls in | policy, with or without a classifier | poor wording |
| Quiet hours and rate limits | policy, by rule | a delayed message |

### A classifier can choose a category. It must not decide terminality

Terminality is the runtime's. A classifier that decides it makes the end of a run
probabilistic, and its failure is unrecoverable. A classifier that chooses only a
category costs poor wording when it is wrong.

A `run.waiting` event carries the same fields whatever the question is. A classifier can
separate a question from an approval request from a blocked run, so that the message can
be routed and worded. The classifier returns a label. The message comes from a template
for each label.

### The classifier needs content that the record does not carry

The payload rule forbids content in the record. A decision about a question needs the
content. The classifier must therefore read the run's content from the runtime over a
separate interface, and that interface does not exist yet.

Prefer the smallest input that answers the question. The last assistant message is enough
to separate a question from an approval request. The whole transcript is not needed, and
it is far more sensitive.

Whoever classifies reads tenant content. A hosted classifier therefore sends tenant
content to a third party. Decide that deliberately rather than by default.

### Failure and suppression

- Every notifiable event is already a pause or an end, so classification is never on a
  run's critical path.
- A classifier that is slow or unavailable must not stop a notification. Send the
  generic message for the event kind.
- A suppression decision is recorded. A person who asks why they were not told must get
  an answer.

## Invariants

These are testable. Each one has a test.

1. A repeated `event_id` produces one row.
2. `sequence` never decreases and is unique.
3. A run's events have ascending sequences.
4. A delivery is never made twice for one sink and one event without the same
   `event_id`.
5. An event is not delivered to a subscription whose principal is not entitled to its
   workspace.
6. An `attributes` map contains no value from the forbidden list.
7. Deleting a workspace removes its runs, events and subscriptions.
8. A callback to a private address is refused.

## Out of scope

- The run state machine's semantics. The runtime owns them.
- The transcript. It stays on the workspace volume.
- Notification content, timing and quiet hours. town owns the message.
- The MCP protocol surface itself. See the MCP specification version `2026-07-28`.

## Open questions

1. Whether a `failed` delivery is visible to the subscriber. A subscriber that cannot
   see the failure cannot act on it.
2. Whether `budget_exhausted` needs a sub-reason, such as tokens against wall-clock.
3. The interface that lets a classifier read the smallest useful part of a run. Nothing
   defines it yet.
4. How the run summary is bounded. It grows without limit for a long-lived workspace,
   unlike the event rows.
5. The name and the payload of the state message the agent sends. The runtime protocol
   defines both.
6. Whether the workspace registry changes its timestamps to epoch milliseconds. The two
   files do not join, so this is a consistency choice and not a correctness one.
