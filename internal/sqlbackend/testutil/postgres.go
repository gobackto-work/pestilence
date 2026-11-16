// Package testutil creates isolated PostgreSQL schemas for persistence tests.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // Register the driver for isolated test schemas.
)

// PostgresSchema uses only the explicitly configured test database.
func PostgresSchema(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PESTILENCE_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Fatal("PESTILENCE_TEST_POSTGRES_URL is required")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		t.Fatal("PESTILENCE_TEST_POSTGRES_URL must be an absolute PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "pestilence_test_" + hex.EncodeToString(random[:])
	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA "`+schema+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated test schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
		_ = admin.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
