// Package store opens the SQLite database and applies the schema.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/rnkch/flimmerkiste/internal/store/db"
)

//go:generate go tool sqlc generate -f ../../sqlc.yaml

//go:embed schema.sql
var schema string

type Store struct {
	DB *sql.DB
	*db.Queries
}

// Open opens (and if needed creates) the database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
		path,
	)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection avoids SQLITE_BUSY
	// between goroutines and is plenty for a single display.
	conn.SetMaxOpenConns(1)
	if err := migrate(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{DB: conn, Queries: db.New(conn)}, nil
}

// Tx runs fn inside a transaction.
func (s *Store) Tx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(s.Queries.WithTx(tx)); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.DB.Close() }
