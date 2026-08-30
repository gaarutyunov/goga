package pgxdb

import (
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/telemetry"
)

// settings is unexported, so that no package outside this one can name it,
// construct it or embed it. The only way a populated settings exists is
// [goga.Apply] folding a caller's options over [defaults].
//
// There is deliberately no field here that could turn instrumentation off. The
// tracer installed in open does not read this struct to decide whether to
// happen.
type settings struct {
	// maxConns and minConns are zero when the caller did not set them, and a
	// zero is left alone rather than written onto the configuration: pgxpool's
	// own defaults — max(4, NumCPU) open, none idle — are reasonable, and they
	// can also be set in the DSN itself (pool_max_conns, pool_min_conns), which
	// a zero here must not silently overwrite.
	maxConns int32
	minConns int32

	instr *telemetry.Instrumentation
}

// defaults returns the settings an [Open] with no options runs with.
func defaults() settings {
	return settings{instr: telemetry.For(moduleName)}
}

// applyTo writes the settings a caller actually set onto the pool
// configuration, leaving everything else — including whatever the DSN's own
// pool_* parameters put there — as pgxpool parsed it.
func (s *settings) applyTo(cfg *pgxpool.Config) {
	if s.maxConns > 0 {
		cfg.MaxConns = s.maxConns
	}
	if s.minConns > 0 {
		cfg.MinConns = s.minConns
	}
}

// Option configures [Open]. It is an exported alias over an unexported settings
// type, so a caller can hold and pass a pgxdb.Option and cannot write the struct
// it mutates.
type Option = goga.Option[settings]

// WithMaxConns bounds the size of the pool.
//
// The default is pgx's own: the greater of four and the number of CPUs. It can
// also be set in the DSN as pool_max_conns, and this option overrides that.
func WithMaxConns(n int) Option {
	return func(s *settings) error {
		if n <= 0 || n > math.MaxInt32 {
			return fmt.Errorf("goga/database/pgxdb: max conns must be in [1, %d], got %d", math.MaxInt32, n)
		}
		s.maxConns = int32(n) //nolint:gosec // the range check above is exactly the conversion's precondition.
		return nil
	}
}

// WithMinConns keeps at least this many connections in the pool.
//
// The default is pgx's own, which is none. Raising it trades idle connections on
// the server for a lower tail latency on the first request after a quiet period.
// It can also be set in the DSN as pool_min_conns, and this option overrides
// that.
func WithMinConns(n int) Option {
	return func(s *settings) error {
		if n <= 0 || n > math.MaxInt32 {
			return fmt.Errorf("goga/database/pgxdb: min conns must be in [1, %d], got %d", math.MaxInt32, n)
		}
		s.minConns = int32(n) //nolint:gosec // the range check above is exactly the conversion's precondition.
		return nil
	}
}

// WithTelemetry replaces the instrumentation handle goga's own spans — the
// construction span and the transaction span — are recorded on.
//
// It replaces; it can never disable. There is no option, and no value of any
// option, that yields an uninstrumented pool: the otelpgx tracer is installed on
// the pool configuration in [Open] without consulting the settings, and a nil
// handle is rejected here rather than accepted as a way to ask for silence.
func WithTelemetry(i *telemetry.Instrumentation) Option {
	return func(s *settings) error {
		if i == nil {
			return errNilTelemetry
		}
		s.instr = i
		return nil
	}
}
