package db

import (
	"context"
	"fmt"
	"log"
)

// WithTx executes fn inside a transaction on a dedicated connection.
// Uses BEGIN IMMEDIATE for SQLite or plain BEGIN for Postgres.
// If fn returns an error, the transaction is rolled back.
func WithTx(ctx context.Context, db *CompatDB, fn func(conn *CompatConn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, db.BeginTxSQL()); err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	if err := fn(conn); err != nil {
		if _, rbErr := conn.ExecContext(ctx, "ROLLBACK"); rbErr != nil {
			log.Printf("rollback failed: %v (original error: %v)", rbErr, err)
		}
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
