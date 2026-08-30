// Package pgxpool is a fixture stub of pgx's connection pool, declaring only
// the surface gogadatabase's fixtures name. analysistest loads its fixture tree
// in GOPATH mode, so a third-party import has to exist here rather than in
// go.mod; it is not a model of the real package.
package pgxpool

import "context"

// Pool is the handle pgxdb.Open returns.
type Pool struct{}

// Close releases the pool's connections.
func (p *Pool) Close() {}

// Config is the parsed form of a connection string.
type Config struct {
	// MaxConns bounds the pool.
	MaxConns int32
}

// New opens a pool from a connection string.
func New(_ context.Context, _ string) (*Pool, error) { return &Pool{}, nil }

// NewWithConfig opens a pool from an already-parsed configuration.
func NewWithConfig(_ context.Context, _ *Config) (*Pool, error) { return &Pool{}, nil }

// ParseConfig parses a connection string. It opens nothing, which is why the
// rule leaves it alone.
func ParseConfig(_ string) (*Config, error) { return &Config{}, nil }
