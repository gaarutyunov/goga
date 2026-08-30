package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/telemetry"
)

var (
	// errNilDB answers a Tx called without a handle.
	errNilDB = errors.New("goga/database: db must not be nil")

	// errNilTxFunc answers a Tx called without a body.
	errNilTxFunc = errors.New("goga/database: transaction function must not be nil")
)

// txSettings is unexported for the same reason [settings] is: the only way a
// populated one exists is [goga.Apply] folding a caller's options over
// [txDefaults].
type txSettings struct {
	timeout  time.Duration
	iso      sql.IsolationLevel
	readOnly bool

	instr *telemetry.Instrumentation
}

// txDefaults returns the settings a [Tx] with no options runs with.
//
// The timeout defaults to zero, which means the transaction is bounded by the
// context it was given and by nothing else. That is deliberately unlike
// goga/serve, where every timeout has a non-zero default: a server's timeouts
// bound what a remote client can do to the process, while a transaction's
// duration is decided by application code that goga cannot second-guess — a
// request handler wants a second and a nightly reconciliation wants an hour.
// What the module does guarantee is that a bound, once set, covers the whole
// transaction rather than each statement inside it.
//
// The isolation level is zero, [sql.LevelDefault], which asks the driver for the
// server's own default — READ COMMITTED on PostgreSQL.
func txDefaults() txSettings {
	return txSettings{instr: telemetry.For(moduleName)}
}

// TxOption configures [Tx].
type TxOption = goga.Option[txSettings]

// WithTxTimeout bounds the transaction as a whole.
//
// The bound is applied once, to the context the body and every statement in it
// share, so a transaction cannot outlive its budget one statement at a time —
// which is what happens when a timeout is applied per query instead.
//
// The default is no bound beyond the caller's own context.
func WithTxTimeout(d time.Duration) TxOption {
	return func(s *txSettings) error {
		if d <= 0 {
			return fmt.Errorf("goga/database: transaction timeout must be > 0, got %s", d)
		}
		s.timeout = d
		return nil
	}
}

// WithTxIsolation runs the transaction at the given isolation level.
//
// The default is [sql.LevelDefault], which asks the server for its own — READ
// COMMITTED on PostgreSQL. A level the driver cannot honour is rejected by the
// driver when the transaction begins, not here: which levels are supported is
// the driver's fact, and duplicating the list would only let the two disagree.
func WithTxIsolation(level sql.IsolationLevel) TxOption {
	return func(s *txSettings) error {
		s.iso = level
		return nil
	}
}

// WithTxReadOnly marks the transaction read-only, so that PostgreSQL rejects any
// write inside it and can route it to a replica.
func WithTxReadOnly(readOnly bool) TxOption {
	return func(s *txSettings) error {
		s.readOnly = readOnly
		return nil
	}
}

// Tx runs fn inside a transaction, committing when it returns nil and rolling
// back when it returns an error or panics.
//
// It is a free function over *sql.DB rather than a method on a wrapper, so the
// type flowing through the application is unchanged by using it: a repository
// that takes a *sql.DB keeps taking a *sql.DB, and the body is handed the
// standard library's *sql.Tx.
//
// The error fn returns is returned unchanged, so a caller's own sentinels still
// match under errors.Is. Only goga's own failures — beginning, committing, or a
// rollback that itself failed — are wrapped, and a failed rollback is joined to
// the body's error rather than replacing it.
//
// A panic in fn rolls the transaction back and then continues to panic with the
// original value. That ordering is the whole reason this helper exists in one
// place: a hand-written transaction wrapper that defers only a rollback-on-error
// leaves a panicking request holding an open transaction, and open transactions
// on PostgreSQL block vacuum and hold locks until the connection is reclaimed.
//
// The context handed to fn carries the transaction's span and, when
// [WithTxTimeout] was given, its deadline. Use it for every statement in the
// body; a statement run on the caller's original context is outside the bound.
//
// [github.com/gaarutyunov/goga/database/pgxdb.Tx] is the same function for pgx's
// transaction type. The two are separate because pgx.Tx and *sql.Tx are
// different types and the point of this module is to stop pretending otherwise;
// their commit, rollback, panic and timeout behaviour is identical and tested to
// stay identical.
func Tx(ctx context.Context, db *sql.DB, fn func(context.Context, *sql.Tx) error, opts ...TxOption) (err error) {
	if db == nil {
		return errNilDB
	}
	if fn == nil {
		return errNilTxFunc
	}

	set, err := goga.Apply(txDefaults(), opts...)
	if err != nil {
		return fmt.Errorf("goga/database: tx: %w", err)
	}

	ctx, end := set.instr.Start(ctx, "Tx")
	defer func() { end(err) }()

	if set.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, set.timeout)
		defer cancel()
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: set.iso, ReadOnly: set.readOnly})
	if err != nil {
		return fmt.Errorf("goga/database: tx: begin: %w", err)
	}

	// Registered before fn runs, so that it is in place for a panic from the
	// very first statement. It re-panics with the original value: the caller's
	// recover sees what its code raised, not something goga wrapped around it.
	// Assigning err first is what gets the panic onto the span, since the
	// deferred end(err) above still runs while the goroutine is panicking.
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		err = fmt.Errorf("goga/database: tx: panic: %v", p)
		if rbErr := rollback(tx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
		panic(p)
	}()

	if ferr := fn(ctx, tx); ferr != nil {
		err = ferr
		if rbErr := rollback(tx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("goga/database: tx: commit: %w", err)
	}
	return nil
}

// rollback rolls tx back, reporting a failure that is worth reporting.
//
// [sql.ErrTxDone] is not one: it means the transaction is already finished —
// the body committed or rolled back itself, or the context deadline killed it —
// and surfacing that beside the real error would tell the caller that cleanup
// failed when it had already happened.
func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("goga/database: tx: rollback: %w", err)
	}
	return nil
}
