package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/schema/*.sql migrations/seed/*.sql
var migrations embed.FS

// MigrateOptions select what `api migrate` applies besides the schema.
type MigrateOptions struct {
	Seed bool // load the demo data set (its own version table, idempotent)
}

// Migrate applies every pending schema migration and, when asked, the demo
// seed. Concurrent runs (a retried Job, two hooks) serialize on a Postgres
// session lock, so a migration never runs twice.
func Migrate(ctx context.Context, pool *pgxpool.Pool, opts MigrateOptions) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	if err := up(ctx, db, "migrations/schema", goose.DefaultTablename); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	if !opts.Seed {
		return nil
	}
	if err := up(ctx, db, "migrations/seed", "goose_seed_version"); err != nil {
		return fmt.Errorf("migrate seed: %w", err)
	}
	return nil
}

func up(ctx context.Context, db *sql.DB, dir, table string) error {
	files, err := fs.Sub(migrations, dir)
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files,
		goose.WithTableName(table), goose.WithSessionLocker(locker))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

// EnsureDatabase creates the database named in dsn when it does not exist.
// It connects to the server's maintenance database with the same credentials,
// so the credentials must be allowed to create databases (the RDS master user
// is). Concurrent callers are fine: when CREATE DATABASE loses a race (the
// database already exists, or the template database is busy with the other
// creation), it checks again and succeeds if the database is there.
func EnsureDatabase(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("postgres: DATABASE_URL is not a valid connection string")
	}
	name := cfg.Database
	admin := cfg.Copy()
	admin.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, admin)
	if err != nil {
		return fmt.Errorf("postgres: connect to maintenance database on %s: %s", cfg.Host, redact(err, cfg.Password))
	}
	defer func() { _ = conn.Close(ctx) }()
	exists, err := databaseExists(ctx, conn, name)
	if err != nil {
		return fmt.Errorf("postgres: check database %q: %s", name, redact(err, cfg.Password))
	}
	if exists {
		return nil
	}
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	if err != nil && lostCreateRace(err) {
		if exists, checkErr := databaseExists(ctx, conn, name); checkErr == nil && exists {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("postgres: create database %q: %s", name, redact(err, cfg.Password))
	}
	return nil
}

func databaseExists(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	return exists, err
}

// lostCreateRace reports whether CREATE DATABASE failed because another
// caller was creating the same database at the same time.
func lostCreateRace(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P04", // duplicate_database
		"23505", // unique_violation on pg_database's name index
		"55006": // object_in_use: the template database is busy with the other creation
		return true
	}
	return false
}
