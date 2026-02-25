package db

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

func RunMigrations(rawDB *sql.DB, dialect Dialect) error {
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

	if _, err := rawDB.Exec(createTableSQL); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	// Backfill logic for existing DBs migrating from the old schema-less version
	var hasUsers int
	if err := rawDB.QueryRow("SELECT 1 FROM users LIMIT 1").Scan(&hasUsers); err == nil {
		if dialect == DialectPostgres {
			rawDB.Exec("INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING", "001_init.sql")
		} else {
			rawDB.Exec("INSERT OR IGNORE INTO schema_migrations (version) VALUES (?)", "001_init.sql")
		}

		var hasScout int
		if err := rawDB.QueryRow("SELECT 1 FROM user_preferences WHERE scout_threshold IS NOT NULL LIMIT 1").Scan(&hasScout); err == nil || strings.Contains(err.Error(), "no such column") == false {
			hasCol := true
			if err != nil {
				if strings.Contains(err.Error(), "no such column") || strings.Contains(err.Error(), "column \"scout_threshold\" does not exist") {
					hasCol = false
				}
			}
			if hasCol {
				if dialect == DialectPostgres {
					rawDB.Exec("INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING", "002_add_scout_prefs.sql")
				} else {
					rawDB.Exec("INSERT OR IGNORE INTO schema_migrations (version) VALUES (?)", "002_add_scout_prefs.sql")
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
			checkErr = rawDB.QueryRow("SELECT 1 FROM schema_migrations WHERE version = $1", file).Scan(&applied)
		} else {
			checkErr = rawDB.QueryRow("SELECT 1 FROM schema_migrations WHERE version = ?", file).Scan(&applied)
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

		tx, err := rawDB.Begin()
		if err != nil {
			return fmt.Errorf("begin transaction for migration %s: %w", file, err)
		}

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

		log.Printf("Applied migration: %s", file)
	}

	return nil
}
