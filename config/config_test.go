package config_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/gaarutyunov/goga/config"
	"github.com/gaarutyunov/goga/semconv"
	"github.com/gaarutyunov/goga/serve/servetest"
)

// app is the configuration struct the tests decode into. It carries one field
// per decoding rule the module promises: a plain string, a duration, a slice,
// and a nested struct reached by a key path.
type app struct {
	Addr     string        `koanf:"addr"`
	Timeout  time.Duration `koanf:"timeout"`
	Tags     []string      `koanf:"tags"`
	Database database      `koanf:"database"`
}

type database struct {
	DSN      string `koanf:"dsn"`
	MaxConns int    `koanf:"max_conns"`
}

// envPrefix is the prefix every test that reads the environment uses. The
// variables are set with t.Setenv, so they are removed when the test that set
// them returns.
const envPrefix = "GOGATEST"

// writeFile writes body to a file named name in a fresh temporary directory and
// returns its path.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// -----------------------------------------------------------------------------
// Precedence: the reason the module exists.
// -----------------------------------------------------------------------------

// sourceName is one of the four kinds of source, used both as the thing under
// test and as the value that source writes — so that the expected result of a
// combination is simply the name of the source that should have won.
type sourceName string

const (
	fromDefaults sourceName = "defaults"
	fromFile     sourceName = "file"
	fromEnv      sourceName = "env"
	fromFlags    sourceName = "flags"
)

// option builds the option that makes s set addr to its own name.
func (s sourceName) option(t *testing.T) config.Option {
	t.Helper()

	switch s {
	case fromDefaults:
		return config.WithDefaults(map[string]any{"addr": string(fromDefaults)})
	case fromFile:
		return config.WithFile(writeFile(t, "app.yaml", "addr: "+string(fromFile)+"\n"))
	case fromEnv:
		t.Setenv(envPrefix+"__ADDR", string(fromEnv))
		return config.WithEnv(envPrefix)
	case fromFlags:
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		fs.String("addr", "flag-default", "")
		require.NoError(t, fs.Parse([]string{"--addr=" + string(fromFlags)}))
		return config.WithFlags(fs)
	default:
		t.Fatalf("unknown source %q", s)
		return nil
	}
}

// TestPrecedenceIsFixed is checklist item 3.1, asserted as a table because
// three projects in this workspace got this wrong in three different ways. The
// order under test is defaults → file → env → flags, and the winner of any
// combination is its highest-ranked member.
func TestPrecedenceIsFixed(t *testing.T) {
	tests := map[string]struct {
		sources []sourceName
		want    sourceName
	}{
		"defaults alone":              {[]sourceName{fromDefaults}, fromDefaults},
		"file alone":                  {[]sourceName{fromFile}, fromFile},
		"env alone":                   {[]sourceName{fromEnv}, fromEnv},
		"flags alone":                 {[]sourceName{fromFlags}, fromFlags},
		"file over defaults":          {[]sourceName{fromDefaults, fromFile}, fromFile},
		"env over defaults":           {[]sourceName{fromDefaults, fromEnv}, fromEnv},
		"env over file":               {[]sourceName{fromFile, fromEnv}, fromEnv},
		"flags over file":             {[]sourceName{fromFile, fromFlags}, fromFlags},
		"flags over env":              {[]sourceName{fromEnv, fromFlags}, fromFlags},
		"the whole chain":             {[]sourceName{fromDefaults, fromFile, fromEnv}, fromEnv},
		"every source sets the key":   {[]sourceName{fromDefaults, fromFile, fromEnv, fromFlags}, fromFlags},
		"defaults and flags, no file": {[]sourceName{fromDefaults, fromFlags}, fromFlags},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			opts := make([]config.Option, 0, len(tt.sources))
			for _, s := range tt.sources {
				opts = append(opts, s.option(t))
			}

			cfg, err := config.Load[app](context.Background(), opts...)
			require.NoError(t, err)

			assert.Equal(t, string(tt.want), cfg.Value.Addr)
		})
	}
}

// TestPrecedenceIgnoresOptionOrder is the regression test for item 3.1 and the
// most important test in the module. Every one of the 24 orders in which the
// four options can be passed must produce the identical result.
//
// An implementation that merged sources in call order passes the table above
// and fails here, which is exactly the failure mode this test exists for: the
// call order in real code is whatever reads best at the call site, and it is
// nobody's idea of a precedence declaration.
func TestPrecedenceIgnoresOptionOrder(t *testing.T) {
	// Built once, outside the permutation loop, so that every ordering is
	// handed the same four options rather than four freshly built ones.
	t.Setenv(envPrefix+"__ADDR", string(fromEnv))

	path := writeFile(t, "app.yaml", "addr: "+string(fromFile)+"\n")

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("addr", "flag-default", "")
	require.NoError(t, flags.Parse([]string{"--addr=" + string(fromFlags)}))

	named := []struct {
		name string
		opt  config.Option
	}{
		{"WithDefaults", config.WithDefaults(map[string]any{"addr": string(fromDefaults)})},
		{"WithFile", config.WithFile(path)},
		{"WithEnv", config.WithEnv(envPrefix)},
		{"WithFlags", config.WithFlags(flags)},
	}

	orders := map[string]bool{}

	permute(len(named), func(order []int) {
		names := make([]string, 0, len(order))
		opts := make([]config.Option, 0, len(order))
		for _, i := range order {
			names = append(names, named[i].name)
			opts = append(opts, named[i].opt)
		}

		orders[strings.Join(names, "+")] = true

		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			cfg, err := config.Load[app](context.Background(), opts...)
			require.NoError(t, err)

			assert.Equal(t, string(fromFlags), cfg.Value.Addr,
				"option order must not affect precedence")
		})
	})

	assert.Len(t, orders, 24,
		"all 4! orders were exercised; a permutation generator that repeats "+
			"itself would make this test look thorough while covering four cases")
}

// TestAFlagLeftAtItsDefaultDoesNotOverrideAFile pins the one part of precedence
// that is genuinely subtle, and the one that has been implemented backwards in
// this workspace: a flag the user did not pass is a fallback, not an override.
func TestAFlagLeftAtItsDefaultDoesNotOverrideAFile(t *testing.T) {
	path := writeFile(t, "app.yaml", "addr: from-file\n")

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("addr", "flag-default", "")
	flags.String("database.max-conns", "7", "")
	require.NoError(t, flags.Parse(nil))

	cfg, err := config.Load[app](context.Background(),
		config.WithFile(path),
		config.WithFlags(flags),
	)
	require.NoError(t, err)

	assert.Equal(t, "from-file", cfg.Value.Addr,
		"an unchanged flag must not override a source that set the key")
	assert.Equal(t, 7, cfg.Value.Database.MaxConns,
		"an unchanged flag still fills a key nothing else set, "+
			"and its name maps to the same key path the env convention produces")
}

// -----------------------------------------------------------------------------
// Files.
// -----------------------------------------------------------------------------

func TestAnAbsentFileIsNotAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	cfg, err := config.Load[app](context.Background(),
		config.WithDefaults(map[string]any{"addr": ":8080"}),
		config.WithFile(missing),
	)

	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.Value.Addr)
}

func TestAnAbsentRequiredFileIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	_, err := config.Load[app](context.Background(), config.WithRequiredFile(missing))

	require.Error(t, err)
	assert.ErrorIs(t, err, fs.ErrNotExist)
	assert.Contains(t, err.Error(), missing, "the error names the file that was required")
}

func TestAMalformedFileIsAnErrorEvenWhenItIsOptional(t *testing.T) {
	path := writeFile(t, "app.yaml", "addr: [unterminated\n")

	_, err := config.Load[app](context.Background(), config.WithFile(path))

	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
}

func TestAnUnsupportedExtensionFailsAtTheCallSite(t *testing.T) {
	_, err := config.Load[app](context.Background(), config.WithFile("/etc/app.conf"))

	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrUnsupportedFormat)

	var unsupported *config.UnsupportedFormatError
	require.ErrorAs(t, err, &unsupported)
	assert.Equal(t, ".conf", unsupported.Ext)
	assert.Contains(t, unsupported.Supported, ".yaml")
}

func TestTwoFilesLayerInTheOrderTheyWerePassed(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	overlay := filepath.Join(dir, "overlay.yaml")
	require.NoError(t, os.WriteFile(base, []byte("addr: base\ntimeout: 1s\n"), 0o600))
	require.NoError(t, os.WriteFile(overlay, []byte("addr: overlay\n"), 0o600))

	cfg, err := config.Load[app](context.Background(),
		config.WithFile(base),
		config.WithFile(overlay),
	)
	require.NoError(t, err)

	assert.Equal(t, "overlay", cfg.Value.Addr, "the later file wins")
	assert.Equal(t, time.Second, cfg.Value.Timeout, "and the earlier one still contributes")
}

// -----------------------------------------------------------------------------
// The environment convention (item 3.4).
// -----------------------------------------------------------------------------

func TestEnvKeyConvention(t *testing.T) {
	t.Setenv(envPrefix+"__ADDR", ":9000")
	t.Setenv(envPrefix+"__DATABASE__MAX_CONNS", "42")
	t.Setenv(envPrefix+"__DATABASE__DSN", "postgres://x")
	// Neither of these belongs to the prefix, and neither may be read.
	t.Setenv("GOGATESTOTHER__ADDR", ":1")
	t.Setenv("UNRELATED", "nope")

	cfg, err := config.Load[app](context.Background(), config.WithEnv(envPrefix))
	require.NoError(t, err)

	assert.Equal(t, ":9000", cfg.Value.Addr)
	assert.Equal(t, 42, cfg.Value.Database.MaxConns,
		"__ separates path segments and _ is literal inside one")
	assert.Equal(t, "postgres://x", cfg.Value.Database.DSN)
}

func TestEnvPrefixIsNormalised(t *testing.T) {
	t.Setenv(envPrefix+"__ADDR", ":9000")

	for _, prefix := range []string{envPrefix, envPrefix + "_", envPrefix + "__"} {
		t.Run(prefix, func(t *testing.T) {
			cfg, err := config.Load[app](context.Background(), config.WithEnv(prefix))
			require.NoError(t, err)
			assert.Equal(t, ":9000", cfg.Value.Addr)
		})
	}
}

func TestAnEmptyEnvPrefixIsRejected(t *testing.T) {
	for _, prefix := range []string{"", "_", "__"} {
		_, err := config.Load[app](context.Background(), config.WithEnv(prefix))
		require.Error(t, err, "prefix %q", prefix)
		assert.Contains(t, err.Error(), "env prefix must not be empty")
	}
}

// -----------------------------------------------------------------------------
// Decoding (item 3.2).
// -----------------------------------------------------------------------------

func TestDurationsAndSlicesDecodeOutOfTheBox(t *testing.T) {
	path := writeFile(t, "app.yaml", "timeout: 30s\ntags: [alpha, beta]\n")
	t.Setenv(envPrefix+"__DATABASE__MAX_CONNS", "12")

	cfg, err := config.Load[app](context.Background(),
		config.WithFile(path),
		config.WithEnv(envPrefix),
	)
	require.NoError(t, err)

	assert.Equal(t, 30*time.Second, cfg.Value.Timeout)
	assert.Equal(t, []string{"alpha", "beta"}, cfg.Value.Tags)
	assert.Equal(t, 12, cfg.Value.Database.MaxConns,
		"a value that can only arrive as a string still decodes into an int")
}

func TestACommaSeparatedStringDecodesIntoASlice(t *testing.T) {
	t.Setenv(envPrefix+"__TAGS", "alpha,beta,gamma")

	cfg, err := config.Load[app](context.Background(), config.WithEnv(envPrefix))
	require.NoError(t, err)

	assert.Equal(t, []string{"alpha", "beta", "gamma"}, cfg.Value.Tags,
		"an environment variable has no other way to carry a list")
}

// level is a project-specific type: exactly the case [config.WithDecodeHook]
// exists for.
type level int

const (
	levelInfo level = iota
	levelDebug
)

type logging struct {
	Level level `koanf:"level"`
}

func TestACustomDecodeHook(t *testing.T) {
	path := writeFile(t, "app.yaml", "level: debug\n")

	hook := func(from, to reflect.Type, data any) (any, error) {
		if from.Kind() != reflect.String || to != reflect.TypeFor[level]() {
			return data, nil
		}
		s, ok := data.(string)
		if !ok {
			return data, nil
		}
		if s == "debug" {
			return levelDebug, nil
		}
		return levelInfo, nil
	}

	cfg, err := config.Load[logging](context.Background(),
		config.WithFile(path),
		config.WithDecodeHook(mapstructure.DecodeHookFunc(hook)),
	)
	require.NoError(t, err)

	assert.Equal(t, levelDebug, cfg.Value.Level)
}

func TestWithoutTheHookTheSameFileFailsToDecode(t *testing.T) {
	// The negative half of the test above: without the hook the value is not
	// merely wrong, it is a decode error, which is what makes the hook the
	// thing under test rather than a no-op the assertion cannot see.
	path := writeFile(t, "app.yaml", "level: debug\n")

	_, err := config.Load[logging](context.Background(), config.WithFile(path))

	require.Error(t, err)
}

// -----------------------------------------------------------------------------
// Required keys (item 3.5).
// -----------------------------------------------------------------------------

func TestAMissingRequiredKeyNamesTheKey(t *testing.T) {
	path := writeFile(t, "app.yaml", "addr: :8080\n")

	_, err := config.Load[app](context.Background(),
		config.WithFile(path),
		config.WithRequiredKeys("database.dsn", "addr", "database.max_conns"),
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrMissingKeys)

	var missing *config.MissingKeysError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, []string{"database.dsn", "database.max_conns"}, missing.Keys,
		"every missing key is reported, in declaration order, and a key that is set is not")
	assert.Contains(t, err.Error(), `"database.dsn"`)
}

func TestRequiredKeysSatisfiedByAnySource(t *testing.T) {
	t.Setenv(envPrefix+"__DATABASE__DSN", "postgres://x")

	cfg, err := config.Load[app](context.Background(),
		config.WithDefaults(map[string]any{"addr": ":8080"}),
		config.WithEnv(envPrefix),
		config.WithRequiredKeys("addr", "database.dsn"),
	)

	require.NoError(t, err)
	assert.Equal(t, "postgres://x", cfg.Value.Database.DSN)
}

// -----------------------------------------------------------------------------
// The scalar/map key collision.
// -----------------------------------------------------------------------------

// TestKoanfSilentlyDiscardsACollidingKey is not a test of goga. It is the
// evidence for the guard below: run the same two sources through koanf directly
// and one of the keys is simply gone, with no error anywhere.
func TestKoanfSilentlyDiscardsACollidingKey(t *testing.T) {
	k := koanf.New(".")

	require.NoError(t, k.Load(confmap.Provider(map[string]any{"catalog": true}, "."), nil))
	require.Equal(t, true, k.Get("catalog"))

	require.NoError(t,
		k.Load(confmap.Provider(map[string]any{"catalog.base_path": "/srv"}, "."), nil),
		"koanf reports no error for a merge it cannot represent")

	assert.Equal(t, []string{"catalog.base_path"}, k.Keys(),
		"the boolean is simply gone, and nothing anywhere said so")
}

func TestACollidingKeyIsReportedNamingBothKeys(t *testing.T) {
	path := writeFile(t, "app.yaml", "catalog:\n  base_path: /srv\n")

	_, err := config.Load[app](context.Background(),
		config.WithDefaults(map[string]any{"catalog": true}),
		config.WithFile(path),
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, config.ErrCollidingKeys)

	var collision *config.CollidingKeysError
	require.ErrorAs(t, err, &collision)
	assert.Equal(t, "catalog", collision.Scalar)
	assert.Equal(t, "catalog.base_path", collision.Nested)
	assert.Equal(t, "defaults", collision.ScalarSource)
	assert.Equal(t, "file:"+path, collision.NestedSource)
	assert.Contains(t, collision.Error(), `"catalog.enabled"`,
		"the message names the shape that would have worked")
}

func TestACollisionIsReportedInEitherDirection(t *testing.T) {
	// The scalar arriving second — a switch set from the environment over a
	// subtree from a file — is the same defect and must report the same way.
	path := writeFile(t, "app.yaml", "catalog:\n  base_path: /srv\n")
	t.Setenv(envPrefix+"__CATALOG", "true")

	_, err := config.Load[app](context.Background(),
		config.WithFile(path),
		config.WithEnv(envPrefix),
	)

	var collision *config.CollidingKeysError
	require.ErrorAs(t, err, &collision)
	assert.Equal(t, "catalog", collision.Scalar)
	assert.Equal(t, "catalog.base_path", collision.Nested)
	assert.Equal(t, "env:"+envPrefix+"__", collision.ScalarSource)
}

func TestOverridingTheSameKeyIsNotACollision(t *testing.T) {
	path := writeFile(t, "app.yaml", "database:\n  dsn: from-file\n")
	t.Setenv(envPrefix+"__DATABASE__DSN", "from-env")

	cfg, err := config.Load[app](context.Background(),
		config.WithFile(path),
		config.WithEnv(envPrefix),
	)

	require.NoError(t, err)
	assert.Equal(t, "from-env", cfg.Value.Database.DSN)
}

// -----------------------------------------------------------------------------
// The raw handle (item 3.3).
// -----------------------------------------------------------------------------

func TestCutReturnsAWorkingSubtree(t *testing.T) {
	path := writeFile(t, "app.yaml",
		"addr: :8080\ndatabase:\n  dsn: postgres://x\n  max_conns: 9\n")

	cfg, err := config.Load[app](context.Background(), config.WithFile(path))
	require.NoError(t, err)

	require.NotNil(t, cfg.K)
	assert.Equal(t, ":8080", cfg.K.String("addr"), "the raw handle carries the whole tree")

	db := cfg.Cut("database")
	assert.Equal(t, "postgres://x", db.String("dsn"))
	assert.Equal(t, 9, db.Int("max_conns"))
	assert.False(t, db.Exists("addr"), "the subtree is rooted at the path, not filtered")

	var decoded database
	require.NoError(t, db.Unmarshal("", &decoded),
		"a module can decode the subtree without naming the application's struct")
	assert.Equal(t, database{DSN: "postgres://x", MaxConns: 9}, decoded)

	assert.NotNil(t, cfg.Cut("nothing.here"), "an absent path is an empty handle, not nil")
}

// -----------------------------------------------------------------------------
// Watching (item 3.6).
// -----------------------------------------------------------------------------

// events collects what a watch callback saw, safely across goroutines.
type events struct {
	mu   sync.Mutex
	seen []config.Event
}

func (e *events) record(ev config.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, ev)
}

// lastValue returns the addr of the most recent successful event.
func (e *events) lastValue() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.seen) - 1; i >= 0; i-- {
		if v, ok := config.ValueOf[app](e.seen[i]); ok {
			return v.Addr, true
		}
	}
	return "", false
}

// rewrite replaces path's contents atomically, so that a watcher never observes
// a half-written file and the test never depends on a partial read.
func rewrite(t *testing.T, path, body string) {
	t.Helper()
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(body), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func TestWatchFiresOnAFileChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.yaml")
	require.NoError(t, os.WriteFile(path, []byte("addr: first\n"), 0o600))

	var seen events

	cfg, err := config.Load[app](context.Background(),
		config.WithFile(path),
		config.WithWatch(seen.record),
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cfg.Close()) })

	require.Equal(t, "first", cfg.Value.Addr)

	rewrite(t, path, "addr: second\n")

	require.Eventually(t, func() bool {
		v, ok := seen.lastValue()
		return ok && v == "second"
	}, 10*time.Second, 20*time.Millisecond, "the watch callback never saw the new value")

	assert.Equal(t, "first", cfg.Value.Addr,
		"a reload delivers the new value in the event and never mutates the loaded one")
}

func TestWatchReRunsEverySourceSoPrecedenceStillHolds(t *testing.T) {
	// The file that changes loses to the environment at startup. It must still
	// lose after the change: reloading only the changed file would let it win.
	path := filepath.Join(t.TempDir(), "app.yaml")
	require.NoError(t, os.WriteFile(path, []byte("addr: file-first\ntags: [one]\n"), 0o600))
	t.Setenv(envPrefix+"__ADDR", "from-env")

	var seen events

	cfg, err := config.Load[app](context.Background(),
		config.WithFile(path),
		config.WithEnv(envPrefix),
		config.WithWatch(seen.record),
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cfg.Close()) })

	rewrite(t, path, "addr: file-second\ntags: [one, two]\n")

	var reloaded *app
	require.Eventually(t, func() bool {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		for i := len(seen.seen) - 1; i >= 0; i-- {
			v, ok := config.ValueOf[app](seen.seen[i])
			if ok && len(v.Tags) == 2 {
				reloaded = v
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)

	assert.Equal(t, "from-env", reloaded.Addr,
		"the environment still wins after a reload")
	assert.Equal(t, []string{"one", "two"}, reloaded.Tags,
		"and the file's own change is applied")
}

func TestWatchReportsAReloadThatFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.yaml")
	require.NoError(t, os.WriteFile(path, []byte("addr: first\n"), 0o600))

	var seen events

	cfg, err := config.Load[app](context.Background(),
		config.WithFile(path),
		config.WithWatch(seen.record),
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cfg.Close()) })

	rewrite(t, path, "addr: [unterminated\n")

	require.Eventually(t, func() bool {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		for _, ev := range seen.seen {
			if ev.Err != nil {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond, "a bad edit must be reported, not swallowed")
}

func TestCloseIsIdempotentAndSafeWithoutAWatch(t *testing.T) {
	cfg, err := config.Load[app](context.Background())
	require.NoError(t, err)

	require.NoError(t, cfg.Close())
	require.NoError(t, cfg.Close())
}

func TestValueOfRejectsTheWrongType(t *testing.T) {
	value := app{Addr: ":8080"}
	ev := config.Event{Value: &value}

	got, ok := config.ValueOf[app](ev)
	require.True(t, ok)
	assert.Equal(t, ":8080", got.Addr)

	_, ok = config.ValueOf[logging](ev)
	assert.False(t, ok)

	_, ok = config.ValueOf[app](config.Event{Err: errors.New("boom")})
	assert.False(t, ok)
}

// -----------------------------------------------------------------------------
// Telemetry (item 3.7).
// -----------------------------------------------------------------------------

// recordSpans installs a span recorder as the global tracer provider for the
// duration of the test. The recorder is built on the OpenTelemetry API rather
// than the SDK, which goga's depguard rules reserve for goga/telemetry.
func recordSpans(t *testing.T) *servetest.SpanRecorder {
	t.Helper()
	recorder := servetest.NewSpanRecorder()
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return recorder
}

// attributeOf returns the value of key on s.
func attributeOf(s servetest.RecordedSpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestLoadOpensASpanCarryingTheSourcesItUsed(t *testing.T) {
	recorder := recordSpans(t)

	path := writeFile(t, "app.yaml", "addr: from-file\n")
	absent := filepath.Join(t.TempDir(), "absent.yaml")
	t.Setenv(envPrefix+"__ADDR", "from-env")

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("addr", "flag-default", "")
	require.NoError(t, flags.Parse(nil))

	_, err := config.Load[app](context.Background(),
		// Deliberately scrambled again: the attribute reports the order the
		// sources were merged, not the order they were configured.
		config.WithFlags(flags),
		config.WithEnv(envPrefix),
		config.WithFile(absent),
		config.WithFile(path),
		config.WithDefaults(map[string]any{"addr": ":8080"}),
	)
	require.NoError(t, err)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "goga.config.config.Load", spans[0].Name)
	assert.Equal(t, codes.Ok, spans[0].Status)

	sources, ok := attributeOf(spans[0], semconv.ConfigSourcesKey)
	require.True(t, ok, "the load span carries goga.config.sources")
	assert.Equal(t,
		[]string{"defaults", "file:" + path, "env:" + envPrefix + "__", "flags"},
		sources.AsStringSlice(),
		"in the fixed order, and without the optional file that was not there")
}

func TestAFailedLoadEndsItsSpanAsAnError(t *testing.T) {
	recorder := recordSpans(t)

	_, err := config.Load[app](context.Background(),
		config.WithDefaults(map[string]any{"addr": ":8080"}),
		config.WithRequiredKeys("database.dsn"),
	)
	require.Error(t, err)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status,
		"named results and defer func(){ end(err) }(): the closer must observe the error. "+
			"With unnamed results the deferred call sees nil and every failed load "+
			"is recorded as a success")

	sources, ok := attributeOf(spans[0], semconv.ConfigSourcesKey)
	require.True(t, ok, "a load that failed after merging still reports what it merged")
	assert.Equal(t, []string{"defaults"}, sources.AsStringSlice())
}

// -----------------------------------------------------------------------------
// Option validation.
// -----------------------------------------------------------------------------

func TestOptionsValidateTheirInput(t *testing.T) {
	tests := map[string]struct {
		opt  config.Option
		want string
	}{
		"an empty file path":     {config.WithFile(""), "file path must not be empty"},
		"an empty required path": {config.WithRequiredFile(""), "file path must not be empty"},
		"a nil flag set":         {config.WithFlags(nil), "flag set must not be nil"},
		"a nil watch callback":   {config.WithWatch(nil), "watch callback must not be nil"},
		"a nil decode hook":      {config.WithDecodeHook(nil), "decode hook must not be nil"},
		"an empty required key":  {config.WithRequiredKeys("addr", ""), "required key 1 must not be empty"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load[app](context.Background(), tt.opt)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Contains(t, err.Error(), "goga/config")
		})
	}
}

func TestLoadWithNoOptionsYieldsTheZeroValue(t *testing.T) {
	cfg, err := config.Load[app](context.Background())

	require.NoError(t, err)
	assert.Equal(t, app{}, cfg.Value)
	require.NotNil(t, cfg.K)
	assert.Empty(t, cfg.K.Keys())
}

// permute calls fn once with every permutation of the indices [0, n).
func permute(n int, fn func(order []int)) {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}

	// Heap's algorithm, in the form it is usually stated: k-1 swaps around k-1
	// recursions, then one more recursion.
	var recurse func(k int)
	recurse = func(k int) {
		if k == 1 {
			fn(append([]int(nil), order...))
			return
		}
		for i := range k - 1 {
			recurse(k - 1)
			if k%2 == 0 {
				order[i], order[k-1] = order[k-1], order[i]
			} else {
				order[0], order[k-1] = order[k-1], order[0]
			}
		}
		recurse(k - 1)
	}
	recurse(n)
}
