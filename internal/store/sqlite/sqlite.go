// Package sqlite is the driven adapter implementing ports.Store over SQLite.
//
// # Concurrency
//
// SQLITE_BUSY is avoided by arrangement rather than by retrying and hoping.
// Two pools are opened over the same file:
//
//	writer  MaxOpenConns(1)  every mutation, serialised by the pool itself
//	reader  MaxOpenConns(N)  every read, never takes a write lock
//
// In WAL mode readers do not block the writer and the writer does not block
// readers, so the only contention left is writer against writer -- and there
// is only ever one, because the pool will not hand out a second connection.
// A goroutine wanting to write waits in Go's connection queue, which is fair
// and cancellable, instead of colliding inside SQLite and being told to try
// again.
//
// busy_timeout is still set, but as a seatbelt for the one case this
// arrangement does not cover: another process opening the same file. Agentd is
// a single-process daemon, so that is a misconfiguration rather than normal
// operation.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver; no cgo

	"github.com/champion19007/agentd/internal/core/domain"
)

// DefaultTenant is the tenant every v1 row is written under.
//
// Agentd v1 is single-tenant. The column exists, leads every key, and is
// filtered on in every query, so that becoming multi-tenant later is a matter
// of supplying a different value rather than rewriting the schema and every
// statement over it.
const DefaultTenant = "default"

// Options configure a Store.
type Options struct {
	// Path is the database file. It is created if absent.
	Path string

	// Tenant is the tenant identifier rows are written under. Empty means
	// DefaultTenant.
	Tenant string

	// ReadPoolSize is how many concurrent readers to allow. Zero means one
	// per CPU, with a floor of four.
	ReadPoolSize int

	// BusyTimeout is how long SQLite waits on a lock before giving up. Zero
	// means five seconds.
	BusyTimeout time.Duration
}

func (o Options) tenant() string {
	if o.Tenant == "" {
		return DefaultTenant
	}
	return o.Tenant
}

func (o Options) readPool() int {
	if o.ReadPoolSize > 0 {
		return o.ReadPoolSize
	}
	if n := runtime.NumCPU(); n > 4 {
		return n
	}
	return 4
}

func (o Options) busyTimeout() time.Duration {
	if o.BusyTimeout > 0 {
		return o.BusyTimeout
	}
	return 5 * time.Second
}

// Store is the SQLite implementation of ports.Store.
type Store struct {
	writer *sql.DB
	reader *sql.DB
	tenant string
	path   string
	codec  codec
}

// Open opens or creates a database and applies any outstanding migrations.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("sqlite: a database path is required")
	}
	abs, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: resolving %q: %w", opts.Path, err)
	}

	writer, err := open(abs, opts, false)
	if err != nil {
		return nil, err
	}
	// One connection, so every write is serialised before it reaches SQLite.
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	if err := migrate(ctx, writer); err != nil {
		writer.Close()
		return nil, err
	}

	reader, err := open(abs, opts, true)
	if err != nil {
		writer.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(opts.readPool())
	reader.SetMaxIdleConns(opts.readPool())
	reader.SetConnMaxLifetime(0)

	s := &Store{
		writer: writer,
		reader: reader,
		tenant: opts.tenant(),
		path:   abs,
	}

	if err := s.verifyWAL(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// open builds one pool over the file.
func open(path string, opts Options, readOnly bool) (*sql.DB, error) {
	// Pragmas travel in the DSN so that every connection in the pool gets
	// them. Setting them with Exec after opening would configure whichever
	// connection happened to serve that call and leave the rest unconfigured.
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		fmt.Sprintf("_pragma=busy_timeout(%d)", opts.busyTimeout().Milliseconds()),
		"_pragma=foreign_keys(1)",
		// NORMAL is the documented companion to WAL: durable across process
		// crashes, and only at risk from losing power mid-write. For a tool
		// whose worst case is re-running a check, that trade is right.
		"_pragma=synchronous(NORMAL)",
	}
	if readOnly {
		// Belt and braces. The reader pool is only handed to read paths, and
		// this makes a mistake there fail loudly rather than take a write
		// lock the architecture assumes nobody takes.
		pragmas = append(pragmas, "_pragma=query_only(1)")
	}

	dsn := "file:" + url.PathEscape(path) + "?" + strings.Join(pragmas, "&")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening %q: %w", path, err)
	}
	return db, nil
}

// verifyWAL confirms the journal mode actually took. A database that silently
// fell back to a rollback journal would serialise readers against the writer,
// and the first symptom would be mysterious slowness under load rather than an
// error anyone could act on.
func (s *Store) verifyWAL(ctx context.Context) error {
	var mode string
	if err := s.reader.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("sqlite: reading journal mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("sqlite: journal mode is %q, want wal", mode)
	}
	return nil
}

// Close shuts both pools down.
func (s *Store) Close() error {
	var errs []error
	if s.reader != nil {
		errs = append(errs, s.reader.Close())
	}
	if s.writer != nil {
		errs = append(errs, s.writer.Close())
	}
	return errors.Join(errs...)
}

// Path returns the database file.
func (s *Store) Path() string { return s.path }

// Tenant returns the tenant rows are written under.
func (s *Store) Tenant() string { return s.tenant }

// JournalMode reports the database's journal mode, for diagnostics and tests.
func (s *Store) JournalMode(ctx context.Context) (string, error) {
	var mode string
	err := s.reader.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	return mode, err
}

// Retention returns the policy this store applies during maintenance. It is
// the domain default; the store does not invent one of its own.
func (s *Store) Retention() domain.Retention { return domain.DefaultRetention() }
