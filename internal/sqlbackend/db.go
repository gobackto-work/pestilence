// Package sqlbackend adapts the fixed queries used by the two SQL stores.
package sqlbackend

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // Register the PostgreSQL driver.
)

// DB retains database/sql's pooling while adapting placeholder syntax.
type DB struct{ *sql.DB }

// Wrap gives the stores the same query methods for both drivers.
func Wrap(db *sql.DB) *DB { return &DB{DB: db} }

// bind handles question marks in our fixed queries, which contain no question-mark literals.
func bind(query string) string {
	var out strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(n))
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// ExecContext runs a fixed statement with backend-specific placeholders.
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(ctx, bind(query), args...)
}

// QueryContext reads rows using backend-specific placeholders.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, bind(query), args...)
}

// QueryRowContext reads one row using backend-specific placeholders.
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, bind(query), args...)
}

// BeginTx begins a transaction with the same placeholder convention.
func (d *DB) BeginTx(ctx context.Context, options *sql.TxOptions) (*Tx, error) {
	tx, err := d.DB.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx}, nil
}

// Tx preserves transaction boundaries across the two drivers.
type Tx struct{ *sql.Tx }

// ExecContext runs a statement in the transaction.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, bind(query), args...)
}

// QueryRowContext reads one row in the transaction.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, bind(query), args...)
}
