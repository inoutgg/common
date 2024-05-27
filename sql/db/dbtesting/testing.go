// Package dbtesting providers utilities for testing database related code.
package dbtesting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/samber/lo"
	"go.inout.gg/common/env"
	"go.inout.gg/common/must"
)

var (
	_, basepath, _, _ = runtime.Caller(0)
	submodulePath     = filepath.Dir(basepath)
)

const (
	queryFetchAllTables = `
SELECT table_name
FROM information_schema.tables
WHERE table_schema=$1::text;
`
)

func queryTruncateTable(table string) string { return fmt.Sprintf("TRUNCATE %s;", table) }

func queryDropTable(
	table string,
) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s;", table)
}

type Config struct {
	Logger *slog.Logger

	// DatabaseURI is the connection string for the database.
	DatabaseURI string   `env:"DATABASE_URI"`
	Schema      string   `env:"DB_SCHEMA"      envDefault:"public"`
	FilePath    []string `env:"DB_SCHEMA_PATH"                     envSeparator:","`
}

// MustLoadConfig loads the configuration from the environment.
//
// If no paths are provided, the default path ".test.env" in the root of the project.
//
// It panics if there is an error loading the configuration.
func MustLoadConfig(paths ...string) *Config {
	if len(paths) == 0 {
		rootpath := findModuleRoot(submodulePath)
		paths = []string{
			filepath.Join(rootpath, ".test.env"),
			filepath.Join(submodulePath, ".test.env"),
		}
	}

	// TODO: add support for custom logger.
	config := env.MustLoad[Config](paths...)
	config.Logger = slog.Default().With("module", "dbtesting")

	return config
}

// DB is a wrapper around pgxpool.Pool with useful utilities for DB management
// in tests.
type DB struct {
	pool      *pgxpool.Pool
	config    *Config
	closeOnce sync.Once
}

func makePool(cfg *Config) *pgxpool.Pool {
	ctx := context.Background()
	config := must.Must(pgxpool.ParseConfig(cfg.DatabaseURI))
	pool := must.Must(pgxpool.NewWithConfig(ctx, config))

	cfg.Logger.Debug("Ping database", slog.String("uri", config.ConnString()))
	must.Must1(pool.Ping(ctx))

	return pool
}

// Must creates a new DB.
//
// It initializes a new pool with the given config.
//
// It panics if there is an error initializing a connection to the database.
func Must(config *Config) *DB {
	pool := makePool(config)

	return &DB{
		pool,
		config,
		sync.Once{},
	}
}

// Pool returns the underlying pool.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Close closes the DB.
func (db *DB) Close() { db.closeOnce.Do(db.close) }
func (db *DB) close() { db.pool.Close() }

// Init initializes tables in the database by creating a schema provided
// by the config.
func (db *DB) Init(ctx context.Context) error {
	var sql []string
	for _, path := range db.config.FilePath {
		schemaContent, err := readFile(path)
		if err != nil {
			return err
		}

		sql = append(sql, parseSchema(schemaContent)...)
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db/testing: error starting transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var errs []error

	for _, s := range sql {
		_, err := tx.Exec(ctx, s)
		if err != nil {
			errs = append(errs, fmt.Errorf("db/testing: error executing query %s: %w", s, err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db/testing: error committing transaction: %w", err)
	}

	return nil
}

// TruncateTable truncates the given table.
func (db *DB) TruncateTable(ctx context.Context, table string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db/testing: error starting transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := db.truncateTable(ctx, table, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db/testing: error committing transaction: %w", err)
	}

	return nil
}

func (db *DB) truncateTable(ctx context.Context, table string, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, queryTruncateTable(table)); err != nil {
		return fmt.Errorf("db/testing: error truncating table %s: %w", table, err)
	}

	return nil
}

// TruncateTables truncates the given tables.
func (db *DB) TruncateTables(ctx context.Context, tables []string) error {

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db/testing: error starting transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var errs []error
	for _, name := range tables {
		if err := db.truncateTable(ctx, name, tx); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db/testing: error committing transaction: %w", err)
	}

	return nil
}

// TruncateAllTables truncates all tables in the database.
func (db *DB) TruncateAllTables(ctx context.Context) error {
	tables, err := db.fetchAllTables(ctx)
	if err != nil {
		return err
	}

	return db.TruncateTables(ctx, tables)
}

func (db *DB) DropAllTables(ctx context.Context) error {
	tables, err := db.fetchAllTables(ctx)
	if err != nil {
		return err
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db/testing: error starting transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var errs []error
	for _, name := range tables {
		if err := db.dropTable(ctx, name, tx); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db/testing: error committing transaction: %w", err)
	}

	return nil
}

func (db *DB) dropTable(ctx context.Context, table string, tx pgx.Tx) error {
	db.config.Logger.Debug("Drop table", slog.String("table", table))
	if _, err := tx.Exec(ctx, queryDropTable(table)); err != nil {
		return fmt.Errorf("db/testing: error dropping table: %w", err)
	}

	return nil
}

// Reset resets the database by truncating all tables and re-creating them.
func (db *DB) Reset(ctx context.Context) error {
	if err := db.DropAllTables(ctx); err != nil {
		return err
	}

	return db.Init(ctx)
}

func (db *DB) fetchAllTables(ctx context.Context) ([]string, error) {
	var tables []string
	rows, err := db.pool.Query(
		ctx,
		queryFetchAllTables,
		pgtype.Text{String: db.config.Schema, Valid: true},
	)
	if err != nil {
		return tables, fmt.Errorf("db/testing: error fetching tables: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var table pgtype.Text
		if err := rows.Scan(&table); err != nil {
			return tables, fmt.Errorf("db/testing: error scanning table name: %w", err)
		}

		tables = append(tables, table.String)
	}

	return tables, nil
}

// rootpath returns the root path of the module.
// It was copied from the [1]. Attributed to the Go Authors.
//
// 1: https://github.com/golang/go/blob/377646589d5fb0224014683e0d1f1db35e60c3ac/src/cmd/go/internal/modload/init.go#L1565C1-L1583C2
func findModuleRoot(dir string) string {
	if dir == "" {
		panic("dir not set")
	}
	dir = filepath.Clean(dir)

	// Look for enclosing go.mod.
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir
		}
		d := filepath.Dir(dir)
		if d == dir {
			break
		}
		dir = d
	}

	return ""
}

// readFile reads the content of the given file.
func readFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("db/testing: error opening file %s: %w", path, err)
	}
	defer file.Close()

	buf := new(strings.Builder)
	_, err = io.Copy(buf, file)
	if err != nil {
		return "", fmt.Errorf("db/testing: error reading file %s: %w", path, err)
	}

	return buf.String(), nil
}

func parseSchema(schema string) []string {
	return lo.Filter(strings.Split(schema, ";"), func(s string, _ int) bool {
		return strings.TrimSpace(s) != ""
	})
}
