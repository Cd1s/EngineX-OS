package enginedb

import (
	"context"
	"database/sql"
)

// stmtCache prepares each statement text once per import transaction. The
// SQLite driver re-parses and re-compiles the SQL of every Exec, including
// Engine's Track triggers, and that was about 40% of a first import (#182).
// It covers only single-statement SQL; schema scripts stay on the Tx.
type stmtCache struct {
	tx *sql.Tx
	m  map[string]*sql.Stmt
}

func newStmtCache(tx *sql.Tx) *stmtCache {
	return &stmtCache{tx: tx, m: map[string]*sql.Stmt{}}
}

func (c *stmtCache) stmt(ctx context.Context, query string) (*sql.Stmt, error) {
	if s := c.m[query]; s != nil {
		return s, nil
	}
	s, err := c.tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	c.m[query] = s
	return s, nil
}

func (c *stmtCache) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s, err := c.stmt(ctx, query)
	if err != nil {
		return nil, err
	}
	return s.ExecContext(ctx, args...)
}

func (c *stmtCache) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	s, err := c.stmt(ctx, query)
	if err != nil {
		return nil, err
	}
	return s.QueryContext(ctx, args...)
}

func (c *stmtCache) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	s, err := c.stmt(ctx, query)
	if err != nil {
		return c.tx.QueryRowContext(ctx, query, args...) // reports the same error through Scan
	}
	return s.QueryRowContext(ctx, args...)
}

// sqlExecer is what the import's per-row helpers need; both *sql.Tx and
// *stmtCache provide it.
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Close releases the prepared statements; the transaction is untouched.
func (c *stmtCache) Close() {
	for _, s := range c.m {
		s.Close()
	}
	c.m = nil
}
