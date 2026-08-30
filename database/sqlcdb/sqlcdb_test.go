package sqlcdb_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/goga/database"
	"github.com/gaarutyunov/goga/database/pgxdb"
	"github.com/gaarutyunov/goga/database/sqlcdb"
)

// unreachableDSN parses but cannot connect. Nothing here runs a query, so a pool
// that never connects is enough to prove the seam holds.
const unreachableDSN database.DSN = "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"

// Queries is shaped exactly like what sqlc's pgx/v5 mode generates: a struct
// over a DBTX, a New that takes one, and a WithTx that rebinds it to a
// transaction. Its purpose is to be compiled, not to be run.
//
// It is written out here rather than generated because generating it would need
// a schema, a query file and a sqlc.yaml to assert a property that is entirely
// about types. What matters is that the three lines below are the three lines
// sqlc emits, unchanged.
type Queries struct {
	db sqlcdb.DBTX
}

// New is sqlc's constructor.
func New(db sqlcdb.DBTX) *Queries { return &Queries{db: db} }

// WithTx is sqlc's transaction rebinding.
func (q *Queries) WithTx(tx pgx.Tx) *Queries { return &Queries{db: tx} }

// db exists so that the field is read somewhere; sqlc's generated methods read
// it on every query.
func (q *Queries) DB() sqlcdb.DBTX { return q.db }

// TestGeneratedCodeTakesTheInstrumentedPoolUnmodified is the whole of this
// package's promise, and it is a compile-time promise: the pool
// [pgxdb.Open] returns is passed to a generated New with no conversion, no
// adapter, no capability check and no error to handle.
//
// The earlier design needed a New(*database.DB) (DBTX, error) that could fail at
// run time when the adapter underneath turned out not to be pgx. That signature
// is gone because the condition it reported is decided by which package the
// composition root imported, and the compiler already knows that.
func TestGeneratedCodeTakesTheInstrumentedPoolUnmodified(t *testing.T) {
	pool, err := pgxdb.Open(t.Context(), unreachableDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	q := New(pool)

	require.NotNil(t, q)
	assert.Same(t, pool, q.DB(), "the generated code holds the instrumented pool itself")
}

// TestTheSeamAcceptsATransaction: a *Queries rebound onto a transaction is how
// sqlc's generated code is used inside [pgxdb.Tx], so pgx.Tx satisfying DBTX is
// part of the seam and not an incidental extra.
func TestTheSeamAcceptsATransaction(t *testing.T) {
	var (
		pool *pgxpool.Pool
		tx   pgx.Tx // the interface, not a value: this test is about the types
	)

	q := New(pool).WithTx(tx)

	assert.NotNil(t, q)
}

// TestDBTXIsSatisfiedByThePoolAndByATransaction restates the package's
// compile-time assertions in the file a reader opens looking for tests. It
// compiles or it does not; there is nothing to run.
func TestDBTXIsSatisfiedByThePoolAndByATransaction(t *testing.T) {
	var (
		pool *pgxpool.Pool
		tx   pgx.Tx
	)

	var _ sqlcdb.DBTX = pool
	var _ sqlcdb.DBTX = tx

	assert.Nil(t, pool)
	assert.Nil(t, tx)
}
