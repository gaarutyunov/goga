package migrate

import (
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/pressly/goose/v3/lock"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/telemetry"
)

// settings is unexported, so that no package outside this one can name it,
// construct it or embed it. The only way a populated settings exists is
// [goga.Apply] folding a caller's options over [defaults].
type settings struct {
	// fsys holds the migrations. It has no default: the house shape is an
	// embed.FS handed to [WithFS], and a default of "whatever is in ./migrations
	// at run time" is precisely the deployment accident embedding exists to
	// remove. [New] rejects a nil fsys rather than falling back to one.
	fsys fs.FS

	table        string
	dialect      string
	allowMissing bool

	lockTimeout time.Duration
	locker      lock.SessionLocker

	instr *telemetry.Instrumentation
}

const (
	// defaultTable is the version table's name, chosen once here so that three
	// projects adopting goga do not pick three names and lose the ability to
	// answer "what schema is this database on" with one query. It is not
	// goose's own default (goose_db_version): the table records what *this*
	// framework applied, and a goga-managed database and a database somebody
	// once ran the goose CLI against are worth telling apart.
	defaultTable = "goga_db_version"

	// defaultDialect is the one engine goga/database opens. A different
	// dialect is a supported option, not a supported combination — nothing
	// else in goga is tested against one.
	defaultDialect = "postgres"

	// defaultLockTimeout bounds how long a boot waits for the advisory lock.
	// Thirty seconds is longer than a rolling restart's stagger and shorter
	// than any orchestrator's startup probe, so the replica that loses the
	// race waits for the winner rather than failing, and a genuinely stuck
	// lock still surfaces as an error instead of hanging the process.
	defaultLockTimeout = 30 * time.Second

	// lockRetryPeriodSeconds is how often the default locker re-tries
	// pg_try_advisory_lock. goose's option takes whole seconds, and one is its
	// minimum, so this is the finest granularity available.
	lockRetryPeriodSeconds uint64 = 1
)

// defaults returns the settings a [New] with no options runs with.
func defaults() settings {
	return settings{
		table:       defaultTable,
		dialect:     defaultDialect,
		lockTimeout: defaultLockTimeout,
		instr:       telemetry.For(moduleName),
	}
}

// Option configures [New]. It is an exported alias over an unexported settings
// type, so a caller can hold and pass a migrate.Option and cannot write the
// struct it mutates.
type Option = goga.Option[settings]

// WithFS supplies the filesystem the migrations are read from.
//
// This is the house shape, and the argument is meant to be an embed.FS:
//
//	//go:embed migrations/*.sql
//	var migrations embed.FS
//
//	m, err := migrate.New(db, migrate.WithFS(must(fs.Sub(migrations, "migrations"))))
//
// A binary that carries its own schema cannot be deployed without it, which
// removes the whole class of "the migrations directory was not in the image".
// goose reads the root of the filesystem it is given, so a subdirectory of an
// embed.FS is selected with [io/fs.Sub] here rather than with a directory
// option that only one of the two sources could honour.
//
// There is no default. [New] fails when neither this option nor [WithDir] was
// passed, rather than silently reading a directory that happens to exist beside
// the binary.
func WithFS(fsys fs.FS) Option {
	return func(s *settings) error {
		if fsys == nil {
			return errNilFS
		}
		s.fsys = fsys
		return nil
	}
}

// WithDir reads the migrations from a directory on the local filesystem
// instead, as os.DirFS(dir).
//
// It writes the same field [WithFS] does, so the two are one setting and the
// later option wins outright — there is no precedence rule to remember and no
// state in which both are half in effect. Prefer embedding; this is for a
// development loop where the migrations are being edited, and for a tool that
// is handed a path.
func WithDir(dir string) Option {
	return func(s *settings) error {
		if dir == "" {
			return fmt.Errorf("goga/migrate: migrations dir must not be empty")
		}
		s.fsys = os.DirFS(dir)
		return nil
	}
}

// WithTable names the version table goose records applied migrations in.
//
// The default is goga_db_version. Change it only when adopting a database that
// already has a migration history under another name; the point of the default
// is that it is the same everywhere.
func WithTable(name string) Option {
	return func(s *settings) error {
		if name == "" {
			return fmt.Errorf("goga/migrate: version table name must not be empty")
		}
		s.table = name
		return nil
	}
}

// WithDialect selects the SQL dialect goose generates its bookkeeping
// statements in.
//
// The default is postgres, which is the one engine goga/database opens and the
// only one the advisory lock exists for: [WithSessionLocker] would have to be
// supplied too for any other, since a session-level advisory lock is a
// PostgreSQL feature.
func WithDialect(d string) Option {
	return func(s *settings) error {
		if d == "" {
			return fmt.Errorf("goga/migrate: dialect must not be empty")
		}
		s.dialect = d
		return nil
	}
}

// WithAllowMissing applies a migration whose version is older than the newest
// one already applied.
//
// It is off by default, and the default is the conservative one: two branches
// merged in the wrong order produce exactly this shape, and applying the older
// migration after the newer one silently gives two databases different schemas
// from the same migration set. With it off, [Migrator.Up] reports the missing
// version and applies nothing.
func WithAllowMissing(on bool) Option {
	return func(s *settings) error {
		s.allowMissing = on
		return nil
	}
}

// WithLockTimeout bounds how long a run waits for the advisory lock before
// giving up.
//
// The bound is a deadline on the acquisition context, so it holds for any
// [WithSessionLocker] implementation and not only for the default one. The
// default is 30 seconds.
func WithLockTimeout(d time.Duration) Option {
	return func(s *settings) error {
		if d <= 0 {
			return fmt.Errorf("goga/migrate: lock timeout must be > 0, got %s", d)
		}
		s.lockTimeout = d
		return nil
	}
}

// WithSessionLocker replaces the session locker the run is serialised with.
//
// The default is goose's PostgreSQL session-level advisory locker. Replacing it
// is what a non-PostgreSQL dialect needs; it is not a way to switch locking
// off, and a nil locker is rejected here rather than accepted as a request for
// one. A migration run that is not serialised is the failure this module
// exists to prevent.
func WithSessionLocker(l lock.SessionLocker) Option {
	return func(s *settings) error {
		if l == nil {
			return errNilLocker
		}
		s.locker = l
		return nil
	}
}

// newLocker returns the locker a run serialises on: the caller's, or goose's
// PostgreSQL advisory locker configured to retry within the lock timeout.
//
// goose's own retry budget is expressed in whole seconds, so it can only
// approximate the timeout. That is why the acquisition is *also* bounded by a
// context deadline in [Migrator.lock] — the deadline is the real bound, and
// this only keeps the locker from giving up before it.
func (s *settings) newLocker() (lock.SessionLocker, error) {
	if s.locker != nil {
		return s.locker, nil
	}

	// At least one attempt, and at least as many seconds as the timeout allows,
	// so the context deadline is what ends the wait rather than the retry count.
	retries := int64(s.lockTimeout / time.Second)
	if s.lockTimeout%time.Second != 0 {
		retries++
	}
	if retries < 1 {
		retries = 1
	}

	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockTimeout(lockRetryPeriodSeconds, uint64(retries)),
	)
	if err != nil {
		return nil, fmt.Errorf("goga/migrate: building the advisory locker: %w", err)
	}
	return locker, nil
}
