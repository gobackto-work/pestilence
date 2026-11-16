# PostgreSQL persistence prototype

Pestilence can select SQLite or PostgreSQL for the workspace and event stores.
SQLite remains the default. The PostgreSQL backend requires a dedicated database.

## Configuration

| Setting | Meaning |
|---|---|
| `-store-backend=sqlite` | Use the existing `-db` and `-event-db` SQLite paths. |
| `-store-backend=postgres` | Use PostgreSQL for both stores. |
| `PESTILENCE_POSTGRES_URL` | PostgreSQL connection URL. Supply it from a secret. |

The PostgreSQL backend refuses to start without a URL or a database connection.
Use a database role restricted to the Pestilence database.
Configure TLS and certificate verification for the selected deployment.
Do not put the URL or its credentials in logs or source control.

The event store serialises PostgreSQL event writes until each transaction commits.
This prevents a subscriber cursor from passing an uncommitted lower sequence.
This prototype does not provide SQLite-to-PostgreSQL data migration.
Do not switch an existing deployment without a verified migration and rollback procedure.
The prototype creates its tables on startup. It does not include schema version management.

## Verification

Run the existing tests against SQLite:

```sh
go test ./...
```

To exercise PostgreSQL, set `PESTILENCE_TEST_POSTGRES_URL` to a disposable test database.
The test role must have permission to create and drop schemas in that database.
The tests create isolated schemas and delete those schemas after each test.
Do not use a production database for this test.

```sh
PESTILENCE_TEST_POSTGRES_URL='postgres://example@localhost:5432/pestilence_test?sslmode=disable' go test ./... -count=1
```

Use `sslmode=disable` only for an isolated local test database.
The tests cover both stores, the control plane API, and concurrent event writers.
They do not prove deployment recovery, backup, migration or high availability.
