package sqlbackend

import "testing"

func TestBindFixedQueries(t *testing.T) {
	query := "UPDATE runs SET state = ? WHERE id = ? AND owner_id = ?"
	if got := bind(query, false); got != query {
		t.Fatalf("SQLite query = %q", got)
	}
	want := "UPDATE runs SET state = $1 WHERE id = $2 AND owner_id = $3"
	if got := bind(query, true); got != want {
		t.Fatalf("PostgreSQL query = %q, want %q", got, want)
	}
}
