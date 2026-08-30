// Package config loads a project's configuration from defaults, files, the
// environment and command-line flags, merges them in one fixed order, and
// decodes the result into the project's own struct.
//
// # The fixed order is the point
//
// Every source is merged in this order, always:
//
//	defaults  →  files  →  environment  →  flags
//
// Later beats earlier, so a flag beats an environment variable, which beats a
// file, which beats a default. The order is written into [Load]. It is not
// derived from the order the options were passed, it is not configurable, and
// there is no option that changes it:
//
//	// These two calls are the same program.
//	config.Load[App](ctx, config.WithFile("app.yaml"), config.WithEnv("APP"))
//	config.Load[App](ctx, config.WithEnv("APP"), config.WithFile("app.yaml"))
//
// That property is the reason this package exists rather than each project
// wiring koanf itself. Three projects in this workspace wired it themselves and
// three got the precedence wrong, in three different ways — one of them by
// passing pflag a key-rewriting callback that, in the koanf release it was
// written against, replaced the "was this flag actually set?" test, so that
// every flag's DEFAULT silently overrode the config file. None of those
// failures produces an error. They produce a service running on a value nobody
// chose, and an operator editing the file that lost.
//
// The mechanism is structural rather than documentary: an option writes into
// the field for its own kind of source, and [settings.sourcesInHouseOrder]
// reads those fields in a sequence spelled out in Go. There is no ordered list
// of sources in call order for anything to sort, so there is nothing an option
// could reorder even if one wanted to.
//
// Within one kind, order does mean what it looks like: two [WithFile] calls
// layer a base file under an overlay, in the order given.
//
// # Keys
//
// A key path is dot-separated: database.max_conns. One convention maps each
// source onto it, and each is documented at the option that implements it —
// [WithEnv] for GOGA__DATABASE__MAX_CONNS, [WithFlags] for
// --database.max-conns.
//
// A key that has children can never also have a value. `catalog: true` beside
// `catalog.base_path` is not a merge koanf can represent: one of them is
// discarded, silently, with the winner decided by which source merged last. A
// feature switch is therefore `catalog.enabled`, never `catalog`.
// goga/config detects the collision while merging and fails with a
// [CollidingKeysError] naming both keys and both sources, which is the whole
// difference between an error and a credential that quietly stopped being read.
//
// # Wiring it up
//
// [Load] is generic in the configuration struct, and a generic function cannot
// be a wire provider: wire's generator works from concrete types, and there is
// no way to write `config.Load[T]` in a provider set without naming T. So the
// project writes the instantiation — one line — and wire provides everything
// downstream of it:
//
//	func provideConfig(ctx context.Context) (*config.Config[App], error) {
//		return config.Load[App](ctx, config.WithFile("app.yaml"), config.WithEnv("APP"))
//	}
//
//	var Set = wire.NewSet(provideConfig, /* … providers taking *config.Config[App] */)
//
// This is a property of the design, not an omission to be fixed later: making
// Load non-generic would mean handing it an `any` and losing the compile-time
// check that the struct and the keys agree. M9 depends on the one-line shape
// above, and it is recorded here so that M9 does not rediscover it.
//
// # The raw handle
//
// [Config] carries the decoded value and the koanf handle it came from,
// because a subtree is routinely what a downstream constructor wants — a
// database module handed `k.Cut("database")` needs no knowledge of the
// application's struct at all. See [Config.Cut].
package config

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"go.opentelemetry.io/otel/trace"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/semconv"
	"github.com/gaarutyunov/goga/telemetry"
)

const (
	// moduleName is this module's name in goga.module.
	moduleName = "config"
	// opLoad is the operation name of the load span.
	opLoad = "config.Load"
)

// instr is the module's telemetry handle. It is taken at package level rather
// than per call because [Load] is a free function with nothing to hang it on;
// [telemetry.For] resolves through the global providers on every use, so a
// handle taken during package initialisation — necessarily before
// telemetry.Setup — starts emitting as soon as Setup runs.
var instr = telemetry.For(moduleName)

// Config is a loaded configuration: the decoded value, and the koanf handle it
// was decoded from.
//
// Both are exported deliberately. The typed value is what an application reads;
// the handle is what a goga module reads, because a module configured from a
// subtree — [Config.Cut] — needs no knowledge of the application's struct.
// Without the handle that pattern cannot be expressed at all, and every module
// ends up with a field in the application's own configuration type.
type Config[T any] struct {
	// Value is the configuration decoded into T. It is the value as of
	// [Load] and is never mutated afterwards; a reload started by [WithWatch]
	// delivers the new value in its [Event] rather than writing here, so that
	// a reader never races the watcher.
	Value T

	// K is the merged koanf handle, with every source applied in the fixed
	// order. It is safe for concurrent reads.
	K *koanf.Koanf

	// set is the settings Load resolved, kept so that a reload can re-run the
	// identical pipeline rather than a reconstruction of it.
	set settings

	// watcher is the file watcher started by [WithWatch], stopped by
	// [Config.Close]. It is nil when no watch was requested.
	watcher *watcher

	closeOnce sync.Once
	closeErr  error
}

// Cut returns the subtree of the merged configuration rooted at path, as a
// koanf handle of its own.
//
// It is how a module is configured without knowing the application's
// configuration struct: a database module handed `cfg.Cut("database")` reads
// dsn and max_conns, and neither module nor application learns the other's
// shape. An absent path yields an empty handle rather than nil, so the caller
// has nothing to check before reading.
func (c *Config[T]) Cut(path string) *koanf.Koanf { return c.K.Cut(path) }

// Close stops the file watcher [WithWatch] started, and waits for it, so that
// no reload callback can still be running when Close returns.
//
// It is safe to call on a [Config] that is not watching anything, and calling
// it twice is harmless. A configuration that is not watched needs no Close, so
// this is not a resource every caller has to remember; it is the one a caller
// that asked for reloads has to release.
func (c *Config[T]) Close() error {
	c.closeOnce.Do(func() {
		if c.watcher != nil {
			c.closeErr = c.watcher.Close()
		}
	})
	return c.closeErr
}

// Event reports one reload, delivered to the callback [WithWatch] registered.
//
// Exactly one of Value and Err is set. Err carries anything that stopped the
// reload — an unparsable file, a required key that the edit removed — and the
// previously loaded [Config] is untouched, so a bad edit degrades to "the old
// configuration is still running" rather than to a half-applied one.
type Event struct {
	// Path is the file whose change triggered the reload.
	Path string

	// K is the koanf handle rebuilt from every source, in the same fixed
	// order [Load] used. It is nil when the rebuild itself failed.
	K *koanf.Koanf

	// Value holds a *T — the freshly decoded configuration — where T is the
	// type argument of the [Load] that produced this event. It is nil when Err
	// is set. Use [ValueOf] rather than asserting it by hand.
	//
	// It is typed any because [WithWatch] configures the unexported settings
	// struct, which is not generic: a generic option type would have to be
	// named by every caller holding a config.Option, and that is the property
	// the whole option shape exists to avoid.
	Value any

	// Err is what stopped the reload, or nil.
	Err error
}

// ValueOf returns the configuration carried by e, decoded into T.
//
// The second result is false when e carries an error instead of a value, or
// when T is not the type the [Load] that produced e was instantiated with.
func ValueOf[T any](e Event) (*T, bool) {
	v, ok := e.Value.(*T)
	return v, ok
}

// Load reads every configured source, merges them in the fixed goga order —
// defaults, files, environment, flags — and decodes the result into T.
//
// The order is fixed inside this function and is not affected by the order
// opts are passed; see the package doc for why that is the point rather than a
// detail. Load with no options is legal and yields the zero value of T.
//
// Load is generic, which means wire cannot provide it: the project writes a
// one-line provider naming its own configuration type. Again, see the package
// doc.
func Load[T any](ctx context.Context, opts ...Option) (cfg *Config[T], err error) {
	ctx, end := instr.Start(ctx, opLoad)
	defer func() { end(err) }()

	var set settings
	if set, err = goga.Apply(defaults(), opts...); err != nil {
		return nil, fmt.Errorf("goga/config: load: %w", err)
	}

	var (
		k    *koanf.Koanf
		used []string
	)
	if k, used, err = build(&set); err != nil {
		return nil, err
	}

	// The sources actually merged, in the order they were merged. Recorded on
	// the span rather than on the duration histogram: it answers "which source
	// won?", which is a question about one load, and a file path on a metric
	// would multiply the cardinality of every series it touched.
	trace.SpanFromContext(ctx).SetAttributes(semconv.ConfigSources(used))

	if missing := set.missingKeys(k); len(missing) > 0 {
		err = &MissingKeysError{Keys: missing}
		return nil, err
	}

	var value T
	if err = decode(k, &value, set.decodeHooks()); err != nil {
		return nil, err
	}

	cfg = &Config[T]{Value: value, K: k, set: set}
	if set.watch != nil {
		if err = cfg.startWatching(); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// build merges every configured source into a fresh koanf, in house order, and
// returns the handle together with the names of the sources that contributed.
//
// It is separate from [Load] because a reload needs exactly this and nothing
// else: re-running the whole pipeline is what stops a changed file from
// winning a precedence contest at run time that it lost at startup.
func build(s *settings) (*koanf.Koanf, []string, error) {
	var (
		k      = koanf.New(delim)
		guard  = newKeyGuard()
		merged = make([]string, 0, len(s.defaults)+len(s.files)+len(s.envs)+len(s.flagSets))
	)

	for _, src := range s.sourcesInHouseOrder() {
		mp, err := src.read(k)
		if err != nil {
			return nil, nil, fmt.Errorf("goga/config: reading %s: %w", src.name, err)
		}
		if mp == nil {
			// An optional file that is not there. It is not an error and it is
			// not a source that contributed, so it is not recorded as one.
			continue
		}
		if err := guard.add(src.name, mp); err != nil {
			return nil, nil, fmt.Errorf("goga/config: merging %s: %w", src.name, err)
		}
		// Delim "": the map is already nested, whether it came from a parser,
		// from confmap's own unflattening, or from posflag.
		if err := k.Load(confmap.Provider(mp, ""), nil); err != nil {
			return nil, nil, fmt.Errorf("goga/config: merging %s: %w", src.name, err)
		}
		merged = append(merged, src.name)
	}

	return k, merged, nil
}

// decode unmarshals the merged configuration into out.
//
// WeaklyTypedInput is on because every value that arrives from the environment
// or from a flag is a string: without it, GOGA__PORT=8080 fails to decode into
// an int, which would make the environment source useless for anything but
// strings.
func decode[T any](k *koanf.Koanf, out *T, hooks []mapstructure.DecodeHookFunc) error {
	err := k.UnmarshalWithConf("", out, koanf.UnmarshalConf{
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook:       mapstructure.ComposeDecodeHookFunc(hooks...),
			WeaklyTypedInput: true,
			Result:           out,
		},
	})
	if err != nil {
		return fmt.Errorf("goga/config: decoding into %T: %w", *out, err)
	}
	return nil
}

// startWatching begins watching every configured file.
//
// A file that does not exist yet is watched too, because it is its parent
// directory that is watched: a configuration file that appears later is a
// change like any other, and under [WithFile] its absence was never an error to
// begin with.
func (c *Config[T]) startWatching() error {
	if len(c.set.files) == 0 {
		// Nothing to watch. This is not an error: a configuration built only
		// from the environment and flags simply never reloads.
		return nil
	}

	paths := make([]string, 0, len(c.set.files))
	for _, f := range c.set.files {
		paths = append(paths, f.path)
	}

	w, err := newWatcher(paths, c.reloaded, c.watchFailed)
	if err != nil {
		return err
	}
	c.watcher = w

	return nil
}

// reloaded re-runs the pipeline after path changed.
func (c *Config[T]) reloaded(path string) { c.reload(path, nil) }

// watchFailed reports an error from the underlying watcher, which is a failure
// of the watch rather than of a reload and so carries no path.
func (c *Config[T]) watchFailed(err error) {
	c.set.watch(Event{Err: fmt.Errorf("goga/config: watch: %w", err)})
}

// reload re-runs the whole pipeline and delivers the result to the watch
// callback. It never mutates c.
func (c *Config[T]) reload(path string, watchErr error) {
	if watchErr != nil {
		c.set.watch(Event{Path: path, Err: fmt.Errorf("goga/config: watching %s: %w", path, watchErr)})
		return
	}

	k, _, err := build(&c.set)
	if err != nil {
		c.set.watch(Event{Path: path, Err: err})
		return
	}
	if missing := c.set.missingKeys(k); len(missing) > 0 {
		c.set.watch(Event{Path: path, K: k, Err: &MissingKeysError{Keys: missing}})
		return
	}

	var value T
	if err := decode(k, &value, c.set.decodeHooks()); err != nil {
		c.set.watch(Event{Path: path, K: k, Err: err})
		return
	}

	c.set.watch(Event{Path: path, K: k, Value: &value})
}

// keyGuard detects the one merge koanf performs silently and wrongly: a key
// held as a scalar by one source and used as the parent of a nested key by
// another. See [CollidingKeysError].
//
// It has to run BEFORE the merge, because after it the evidence is gone —
// whichever key lost is simply absent from the result, indistinguishable from
// a key nobody set.
type keyGuard struct {
	// leaves maps a leaf key to the source that last set it.
	leaves map[string]string
	// branches maps every parent path to one leaf key below it and the source
	// that introduced it, so that a collision can name a concrete example.
	branches map[string]branch
}

// branch records one nested key that made a path a parent.
type branch struct {
	leaf   string
	source string
}

func newKeyGuard() *keyGuard {
	return &keyGuard{leaves: map[string]string{}, branches: map[string]branch{}}
}

// add records every key one source contributes, and returns a
// [CollidingKeysError] for the first key that cannot coexist with what is
// already recorded.
func (g *keyGuard) add(sourceName string, mp map[string]any) error {
	flat, _ := maps.Flatten(mp, nil, delim)

	keys := make([]string, 0, len(flat))
	for key := range flat {
		keys = append(keys, key)
	}
	// Sorted, so that a source contributing several colliding keys reports the
	// same one every run, and so that a parent is always seen before its
	// children within a single source.
	sort.Strings(keys)

	for _, key := range keys {
		// This key nests under something another source set to a value.
		for _, parent := range parentPaths(key) {
			if owner, ok := g.leaves[parent]; ok {
				return &CollidingKeysError{
					Scalar: parent, ScalarSource: owner,
					Nested: key, NestedSource: sourceName,
				}
			}
		}
		// This key IS something another source used as a parent.
		if b, ok := g.branches[key]; ok {
			return &CollidingKeysError{
				Scalar: key, ScalarSource: sourceName,
				Nested: b.leaf, NestedSource: b.source,
			}
		}

		g.leaves[key] = sourceName
		for _, parent := range parentPaths(key) {
			if _, ok := g.branches[parent]; !ok {
				g.branches[parent] = branch{leaf: key, source: sourceName}
			}
		}
	}

	return nil
}

// parentPaths returns every proper prefix path of key: "a.b.c" yields "a" and
// "a.b". A top-level key has none.
func parentPaths(key string) []string {
	parts := strings.Split(key, delim)
	if len(parts) < 2 {
		return nil
	}
	out := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[:i], delim))
	}
	return out
}
