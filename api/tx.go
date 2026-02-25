package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// withTx executes fn inside a BEGIN IMMEDIATE / COMMIT transaction on a
// dedicated connection. If fn returns an error, the transaction is rolled back.
func withTx(ctx context.Context, db *sql.DB, fn func(conn *sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
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
