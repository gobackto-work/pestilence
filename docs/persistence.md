# PostgreSQL persistence

Pestilence stores workspaces, runs, events and subscriptions in PostgreSQL.
`PESTILENCE_POSTGRES_URL` supplies the connection URL.
The deployment must supply this value from a Kubernetes Secret.
The PostgreSQL role must have access only to the Pestilence database.

The event store serialises writes until each transaction commits.
A subscription cursor must not pass an uncommitted lower sequence.
The event and run summary must commit in the same transaction.

## Migrate the legacy databases

Stop all writers before migration. Preserve both SQLite files for rollback.
Supply an empty PostgreSQL database and `PESTILENCE_POSTGRES_URL`.
Run the migration tool against the offline files:

```sh
migrate-sqlite-postgres -offline-confirmed \
  -workspace-db=/path/to/pestilence.db -event-db=/path/to/events.db
```

The tool refuses a non-empty target. The tool preserves event sequences and cursors.
The tool imports all records in one PostgreSQL transaction.

## Verification

Set `PESTILENCE_TEST_POSTGRES_URL` to a disposable test database.
The test role must have permission to create and drop schemas in that database.
The tests create isolated schemas and delete those schemas after each test.
Do not use a production database for these tests.

```sh
PESTILENCE_TEST_POSTGRES_URL='postgres://example@localhost:5432/pestilence_test?sslmode=disable' go test ./... -count=1
```

Use `sslmode=disable` only for an isolated local test database.
The tests cover the stores, the control plane API and concurrent event writers.
