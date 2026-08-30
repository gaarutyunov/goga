package pgxdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/telemetry"
)

var (
	// errNilPool answers a Tx called without a pool.
	errNilPool = errors.New("goga/database/pgxdb: pool must not be nil")

	// errNilTxFunc answers a Tx called without a body.
	errNilTxFunc = errors.New("goga/database/pgxdb: transaction function must not be nil")
)

// txSettings is unexported for the same reason [settings] is.
type txSettings struct {
	timeout time.Duration
	opts    pgx.TxOptions

	instr *telemetry.Instrumentation
}

// txDefaults returns the settings a [Tx] with no options runs with.
//
// The timeout defaults to zero — the transaction is bounded by the context it
// was given and by nothing else — and the pgx.TxOptions are zero, which asks
// PostgreSQL for its own defaults: READ COMMITTED, read-write, deferrable off.
// This matches [github.com/gaarutyunov/goga/database.Tx] exactly; the two
// helpers' defaults are part of what the shared behaviour test pins.
func txDefaults() txSettings {
	return txSettings{instr: telemetry.For(moduleName)}
}

// TxOption configures [Tx].
type TxOption = goga.Option[txSettings]

// WithTxTimeout bounds the transaction as a whole.
//
// The bound is applied once, to the context the body and every statement in it
// share, so a transaction cannot outlive its budget one statement at a time.
//
// The default is no bound beyond the caller's own context.
func WithTxTimeout(d time.Duration) TxOption {
	return func(s *txSettings) error {
		if d <= 0 {
			return fmt.Errorf("goga/database/pgxdb: transaction timeout must be > 0, got %s", d)
		}
		s.timeout = d
		return nil
	}
}

// WithTxIsolation runs the transaction at the given isolation level.
//
// The level is pgx's own type rather than the standard library's, for the same
// reason this helper exists separately at all: pgx names PostgreSQL's four
// levels directly, and translating them through database/sql's enumeration would
// add a lossy step to reach a value the caller already has.
//
// The default is pgx's zero value, which asks the server for its own —
// READ COMMITTED.
func WithTxIsolation(level pgx.TxIsoLevel) TxOption {
	return func(s *txSettings) error {
		s.opts.IsoLevel = level
		return nil
	}
}

// WithTxReadOnly marks the transaction read-only, so that PostgreSQL rejects any
// write inside it and can route it to a replica.
func WithTxReadOnly(readOnly bool) TxOption {
	return func(s *txSettings) error {
		if readOnly {
			s.opts.AccessMode = pgx.ReadOnly
		} else {
			s.opts.AccessMode = pgx.ReadWrite
		}
		return nil
	}
}

// Tx runs fn inside a transaction on the pool, committing when it returns nil
// and rolling back when it returns an error or panics.
//
// It is the same function as
// [github.com/gaarutyunov/goga/database.Tx] for pgx's transaction type. The two
// are separate rather than one generic helper because pgx.Tx and *sql.Tx are
// different types with different method sets, and the whole point of this
// module is to stop pretending otherwise. Their commit, rollback, panic and
// timeout behaviour is identical, and a test asserts that it stays identical.
//
// The error fn returns is returned unchanged, so a caller's own sentinels still
// match under errors.Is. Only goga's own failures — beginning, committing, or a
// rollback that itself failed — are wrapped, and a failed rollback is joined to
// the body's error rather than replacing it.
//
// A panic in fn rolls the transaction back and then continues to panic with the
// original value. A hand-written wrapper that defers only a rollback-on-error
// leaves a panicking request holding an open transaction, and an open
// transaction on PostgreSQL blocks vacuum and holds locks until the connection
// is reclaimed.
//
// The context handed to fn carries the transaction's span and, when
// [WithTxTimeout] was given, its deadline. Use it for every statement in the
// body; a statement run on the caller's original context is outside the bound.
func Tx(ctx context.Context, pool *pgxpool.Pool, fn func(context.Context, pgx.Tx) error, opts ...TxOption) (err error) {
	if pool == nil {
		return errNilPool
	}
	if fn == nil {
		return errNilTxFunc
	}

	set, err := goga.Apply(txDefaults(), opts...)
	if err != nil {
		return fmt.Errorf("goga/database/pgxdb: tx: %w", err)
	}

	ctx, end := set.instr.Start(ctx, "Tx")
	defer func() { end(err) }()

	if set.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, set.timeout)
		defer cancel()
	}

	tx, err := pool.BeginTx(ctx, set.opts)
	if err != nil {
		return fmt.Errorf("goga/database/pgxdb: tx: begin: %w", err)
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
		err = fmt.Errorf("goga/database/pgxdb: tx: panic: %v", p)
		if rbErr := rollback(ctx, tx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
		panic(p)
	}()

	if ferr := fn(ctx, tx); ferr != nil {
		err = ferr
		if rbErr := rollback(ctx, tx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("goga/database/pgxdb: tx: commit: %w", err)
	}
	return nil
}

// rollback rolls tx back, reporting a failure that is worth reporting.
//
// [pgx.ErrTxClosed] is not one: it means the transaction is already finished —
// the body committed or rolled back itself — and surfacing that beside the real
// error would tell the caller that cleanup failed when it had already happened.
//
// The rollback is issued on a context detached from ctx. That is the difference
// forced by pgx's signature and it is deliberate: pgx.Tx.Rollback takes a
// context, and by the time a rollback is needed the transaction's own context
// may already be cancelled — by [WithTxTimeout] expiring, or by the caller — in
// which case a rollback on it would fail to send and leave the transaction open
// on the server until the connection is reclaimed. The standard library's
// Tx.Rollback takes no context and has the same behaviour for free.
func rollback(ctx context.Context, tx pgx.Tx) error {
	//nolint:contextcheck // detaching is the point: see the doc comment above.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackGrace)
	defer cancel()

	if err := tx.Rollback(rctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("goga/database/pgxdb: tx: rollback: %w", err)
	}
	return nil
}

// rollbackGrace bounds the detached rollback in [rollback]. It exists so that
// the escape from a cancelled context cannot itself become an unbounded wait on
// a connection that will never answer.
const rollbackGrace = 5 * time.Second
