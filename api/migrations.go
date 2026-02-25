package main

import (
	"database/sql"
	"embed"
	"fmt"
	"log"
	"sort"
	"strings"
)

//go:embed migrations/*
var migrationsFS embed.FS

func runMigrations(db *sql.DB, dialect Dialect) error {
	createTableSQL := `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`
	if dialect == DialectPostgres {
		createTableSQL = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		)`
	}

	if _, err := db.Exec(createTableSQL); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	// Backfill logic for existing DBs migrating from the old schema-less version
	var hasUsers int
	if err := db.QueryRow("SELECT 1 FROM users LIMIT 1").Scan(&hasUsers); err == nil {
		if dialect == DialectPostgres {
			db.Exec("INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING", "001_init.sql")
		} else {
			db.Exec("INSERT OR IGNORE INTO schema_migrations (version) VALUES (?)", "001_init.sql")
		}

		var hasScout int
		if err := db.QueryRow("SELECT 1 FROM user_preferences WHERE scout_threshold IS NOT NULL LIMIT 1").Scan(&hasScout); err == nil || strings.Contains(err.Error(), "no such column") == false {
			// If scout_threshold column exists, even if query fails for other reasons, it means we probably have the column
			// Let's do a safer check for SQLite:
			hasCol := true
			if err != nil {
				if strings.Contains(err.Error(), "no such column") || strings.Contains(err.Error(), "column \"scout_threshold\" does not exist") {
					hasCol = false
				}
			}
			if hasCol {
				if dialect == DialectPostgres {
					db.Exec("INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING", "002_add_scout_prefs.sql")
				} else {
					db.Exec("INSERT OR IGNORE INTO schema_migrations (version) VALUES (?)", "002_add_scout_prefs.sql")
				}
			}
		}
	}

	dir := "migrations/" + string(dialect)
	entries, err := migrationsFS.ReadDir(dir)
	if err != nil {
		log.Printf("No migrations directory found: %s", dir)
		return nil
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	for _, file := range files {
		var applied int
		var checkErr error
		if dialect == DialectPostgres {
			checkErr = db.QueryRow("SELECT 1 FROM schema_migrations WHERE version = $1", file).Scan(&applied)
		} else {
			checkErr = db.QueryRow("SELECT 1 FROM schema_migrations WHERE version = ?", file).Scan(&applied)
		}

		if checkErr == nil && applied == 1 {
			continue // Already applied
		}

		path := dir + "/" + file
		content, err := migrationsFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", file, err)
		}

		log.Printf("Applying migration: %s", file)
		
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin transaction for migration %s: %w", file, err)
		}
		
		// Some SQLite statements like PRAGMA cannot run in a transaction,
		// but standard DDL should be fine. We just execute them.
		if _, err := tx.Exec(string(content)); err != nil {
			tx.Rollback()
			return fmt.Errorf("exec migration %s: %w", file, err)
		}
		
		if dialect == DialectPostgres {
			_, err = tx.Exec("INSERT INTO schema_migrations (version) VALUES ($1)", file)
		} else {
			_, err = tx.Exec("INSERT INTO schema_migrations (version) VALUES (?)", file)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", file, err)
		}
		
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", file, err)
		}
	}
	return nil
}
