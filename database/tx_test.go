package database_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/mock/gomock"

	"github.com/gaarutyunov/goga/database"
	"github.com/gaarutyunov/goga/serve/servetest"
)

// mockDriverSeq numbers the registered driver names, because database/sql's
// driver registry is global and process-wide and panics on a duplicate name.
var mockDriverSeq atomic.Int64

// newMockDB returns a *sql.DB over a mocked driver, together with the mocked
// connection and the mocked transaction it will hand out.
//
// The handle is built with sql.Open and not with [database.Open] on purpose:
// [database.Tx] is a free function over the standard library's type, so it has
// to work on any *sql.DB however the caller obtained it, and going through the
// otelsql wrapper here would test the transaction logic through a layer that
// could hide a defect in it. That the wrapper is applied is asserted separately,
// in database_test.go and api_test.go.
//
// The controller is created before the handle so that the cleanups unwind in the
// right order: t.Cleanup runs last-in-first-out, so db.Close — which closes the
// mocked connection — happens before the controller checks its expectations.
func newMockDB(t *testing.T) (*sql.DB, *MockConn, *MockTx) {
	t.Helper()

	ctrl := gomock.NewController(t)

	drv := NewMockDriver(ctrl)
	conn := NewMockConn(ctrl)
	tx := NewMockTx(ctrl)

	// Incidental to every test: database/sql opens a connection when it needs
	// one and closes it when the handle closes.
	drv.EXPECT().Open(gomock.Any()).Return(conn, nil).AnyTimes()
	conn.EXPECT().Close().Return(nil).AnyTimes()

	name := "goga_database_mock_" + strconv.FormatInt(mockDriverSeq.Add(1), 10)
	sql.Register(name, drv)

	db, err := sql.Open(name, "")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	return db, conn, tx
}

func TestTxCommitsWhenTheBodyReturnsNil(t *testing.T) {
	db, conn, tx := newMockDB(t)

	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Commit().Return(nil)
	tx.EXPECT().Rollback().Times(0)

	called := false
	err := database.Tx(t.Context(), db, func(_ context.Context, tx *sql.Tx) error {
		called = true
		assert.NotNil(t, tx, "the body is handed the standard library's transaction")
		return nil
	})

	require.NoError(t, err)
	assert.True(t, called)
}

func TestTxRollsBackWhenTheBodyReturnsAnError(t *testing.T) {
	db, conn, tx := newMockDB(t)

	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Rollback().Return(nil)
	tx.EXPECT().Commit().Times(0)

	sentinel := errors.New("the body failed")
	err := database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error { return sentinel })

	require.ErrorIs(t, err, sentinel,
		"the body's error reaches the caller unchanged, so its own sentinels still match")
}

// TestTxRollsBackOnPanicAndRepanics is the assertion most likely to be missing,
// and the one whose absence costs the most: a helper that defers only a
// rollback-on-error leaves a panicking request holding an open transaction,
// which on PostgreSQL blocks vacuum and holds locks until the connection is
// reclaimed.
func TestTxRollsBackOnPanicAndRepanics(t *testing.T) {
	db, conn, tx := newMockDB(t)

	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Rollback().Return(nil)
	tx.EXPECT().Commit().Times(0)

	assert.PanicsWithValue(t, "boom", func() {
		_ = database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error {
			panic("boom")
		})
	}, "the original panic value continues, so the caller's recover sees what its code raised")
}

// TestTxRollsBackOnAPanickingRuntimeError covers the panic nobody writes on
// purpose — an index off the end of a slice — because that is the shape that
// actually happens in production, and it is raised by the runtime rather than
// by a panic statement the helper could have been written to expect.
func TestTxRollsBackOnAPanickingRuntimeError(t *testing.T) {
	db, conn, tx := newMockDB(t)

	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Rollback().Return(nil)
	tx.EXPECT().Commit().Times(0)

	assert.Panics(t, func() {
		_ = database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error {
			rows := make([]string, 0)
			_ = rows[len(rows)] // index out of range
			return nil
		})
	})
}

// TestTxRecordsAPanicOnItsSpan: the span still ends, and it ends as an error.
// Without this the panic path would be the one case where a transaction
// disappears from the telemetry entirely.
func TestTxRecordsAPanicOnItsSpan(t *testing.T) {
	recorder := recordSpans(t)
	db, conn, tx := newMockDB(t)

	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Rollback().Return(nil)

	assert.Panics(t, func() {
		_ = database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error { panic("boom") })
	})

	span, found := findSpan(recorder, "goga.database.Tx")
	require.True(t, found, "the transaction span ended even though the goroutine was panicking")
	assert.Equal(t, codes.Error, span.Status)
}

func TestTxSurfacesAFailedCommit(t *testing.T) {
	db, conn, tx := newMockDB(t)

	commitErr := errors.New("commit refused")
	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Commit().Return(commitErr)

	err := database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error { return nil })

	require.ErrorIs(t, err, commitErr)
	assert.Contains(t, err.Error(), "commit")
}

func TestTxSurfacesAFailedBegin(t *testing.T) {
	db, conn, _ := newMockDB(t)

	beginErr := errors.New("no connection")
	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(nil, beginErr)

	called := false
	err := database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error {
		called = true
		return nil
	})

	require.ErrorIs(t, err, beginErr)
	assert.False(t, called, "the body never runs when the transaction could not begin")
}

// TestTxJoinsAFailedRollbackToTheBodyError: the body's error is what the caller
// came for, so a rollback that also failed is added beside it rather than
// replacing it.
func TestTxJoinsAFailedRollbackToTheBodyError(t *testing.T) {
	db, conn, tx := newMockDB(t)

	rollbackErr := errors.New("connection gone")
	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Rollback().Return(rollbackErr)

	bodyErr := errors.New("the body failed")
	err := database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error { return bodyErr })

	require.ErrorIs(t, err, bodyErr)
	require.ErrorIs(t, err, rollbackErr)
}

func TestTxRejectsNilArguments(t *testing.T) {
	db, _, _ := newMockDB(t)

	t.Run("nil db", func(t *testing.T) {
		err := database.Tx(t.Context(), nil, func(context.Context, *sql.Tx) error { return nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be nil")
	})

	t.Run("nil function", func(t *testing.T) {
		err := database.Tx(t.Context(), db, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be nil")
	})
}

// TestTxTimeoutBoundsTheWholeTransaction: the deadline is on the context the
// body is handed, so it covers every statement in it rather than each one
// separately. A per-statement bound lets a transaction outlive its budget one
// statement at a time.
func TestTxTimeoutBoundsTheWholeTransaction(t *testing.T) {
	db, conn, tx := newMockDB(t)

	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	// AnyTimes: when the deadline fires, database/sql rolls the transaction back
	// from its own goroutine, and goga's rollback then finds it already done.
	// Which of the two reaches the driver is a race the assertion must not
	// depend on; that the transaction is not committed is not.
	tx.EXPECT().Rollback().Return(nil).AnyTimes()
	tx.EXPECT().Commit().Times(0)

	const budget = 50 * time.Millisecond
	start := time.Now()

	err := database.Tx(t.Context(), db, func(ctx context.Context, _ *sql.Tx) error {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "the body's context carries the transaction's deadline")
		assert.WithinDuration(t, start.Add(budget), deadline, 20*time.Millisecond)

		<-ctx.Done()
		return fmt.Errorf("body observed: %w", ctx.Err())
	}, database.WithTxTimeout(budget))

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestTxPassesIsolationAndAccessModeToTheDriver: the options are not decoration.
func TestTxPassesIsolationAndAccessModeToTheDriver(t *testing.T) {
	db, conn, tx := newMockDB(t)

	var got driver.TxOptions
	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
			got = opts
			return tx, nil
		})
	tx.EXPECT().Commit().Return(nil)

	require.NoError(t, database.Tx(t.Context(), db,
		func(context.Context, *sql.Tx) error { return nil },
		database.WithTxIsolation(sql.LevelSerializable),
		database.WithTxReadOnly(true)))

	assert.Equal(t, driver.IsolationLevel(sql.LevelSerializable), got.Isolation)
	assert.True(t, got.ReadOnly)
}

// TestTxRecordsASpan checks the successful path's telemetry, which is the case
// the panic test above cannot cover.
func TestTxRecordsASpan(t *testing.T) {
	recorder := recordSpans(t)
	db, conn, tx := newMockDB(t)

	conn.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
	tx.EXPECT().Commit().Return(nil)

	require.NoError(t, database.Tx(t.Context(), db, func(context.Context, *sql.Tx) error { return nil }))

	assert.Contains(t, recorder.Names(), "goga.database.Tx")
}

// findSpan returns the last span the recorder saw under name.
func findSpan(rec *servetest.SpanRecorder, name string) (servetest.RecordedSpan, bool) {
	var (
		found bool
		span  servetest.RecordedSpan
	)
	for _, s := range rec.Ended() {
		if s.Name == name {
			span, found = s, true
		}
	}
	return span, found
}
