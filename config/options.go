package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	env "github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"

	"github.com/gaarutyunov/goga"
)

const (
	// delim separates the segments of a key path everywhere in this package:
	// in koanf, in [WithRequiredKeys], in [Config.Cut] and in a flag name.
	//
	// It is a constant and not a setting. A configurable delimiter buys a
	// project nothing and costs it the ability to read any goga documentation
	// literally, and every key convention below — the env separator, the flag
	// name mapping — is stated in terms of it.
	delim = "."

	// envSeparator separates path segments inside an environment variable
	// name. See [WithEnv], which documents the whole convention.
	envSeparator = "__"

	// flagSeparator is the character a POSIX flag name conventionally uses
	// where a key path uses [flagWordSeparator]. See [WithFlags].
	flagSeparator = "-"

	// flagWordSeparator is what a segment of a key path uses between words,
	// matching the env convention: max_conns, not max-conns.
	flagWordSeparator = "_"
)

// parsers maps a file extension to the koanf parser that reads it. It is the
// whole of what [WithFile] accepts, and the list [UnsupportedFormatError]
// reports.
//
// The three formats are the ones a Go service is actually configured with. A
// fourth is a decision, not an omission: every format added here is a parser
// dependency every consumer of goga/config carries, whether or not it uses it.
var parsers = map[string]func() koanf.Parser{
	".yaml": func() koanf.Parser { return yaml.Parser() },
	".yml":  func() koanf.Parser { return yaml.Parser() },
	".json": func() koanf.Parser { return json.Parser() },
	".toml": func() koanf.Parser { return toml.Parser() },
}

// supportedExtensions lists the keys of [parsers], sorted, for error messages.
func supportedExtensions() []string {
	out := make([]string, 0, len(parsers))
	for ext := range parsers {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// settings is unexported, so that no package outside this one can name it,
// construct it or embed it. The only way a populated settings exists is
// [goga.Apply] folding a caller's options over [defaults].
//
// Note what it does NOT have: a single ordered list of sources. Each kind of
// source has a field of its own, and [settings.sourcesInHouseOrder] reads
// those fields in a fixed sequence. That is the whole mechanism behind the
// precedence guarantee — see the package doc, and [Load].
type settings struct {
	defaults []map[string]any
	files    []fileSource
	envs     []envSource
	flagSets []*pflag.FlagSet

	requiredKeys []string
	hooks        []mapstructure.DecodeHookFunc
	watch        func(Event)
}

// defaults returns the settings a [Load] with no options runs with: no
// sources, no required keys, no watch. A Load with no options is legal and
// yields the zero value of T, which is what makes "every setting has a default
// in the struct" a usable style.
func defaults() settings { return settings{} }

// Option configures [Load]. It is an exported alias over an unexported
// settings type, so a caller can hold and pass a config.Option and cannot
// write the struct it mutates.
//
// An option records WHICH source to read. It never records WHEN to read it:
// there is no option that reorders the sources, because there is no ordering
// for an option to change. See the package doc.
type Option = goga.Option[settings]

// sourceKind ranks the four kinds of configuration source. The zero value is
// the lowest-precedence kind on purpose: a source that somehow reached the
// pipeline without a kind loses every conflict rather than winning it.
type sourceKind int

const (
	kindDefaults sourceKind = iota
	kindFile
	kindEnv
	kindFlags
)

// source is one resolved place to read configuration from.
//
// read takes the koanf built from every HIGHER-precedence source so far,
// because one source needs it: pflag cannot tell a flag left at its default
// from a flag explicitly set to that same value without asking what is already
// loaded. Nothing else uses the argument.
type source struct {
	kind sourceKind
	// name is what the goga.config.sources span attribute and the error
	// messages call this source: "defaults", "file:<path>", "env:<prefix>",
	// "flags".
	name string
	read func(k *koanf.Koanf) (map[string]any, error)
}

// sourcesInHouseOrder returns every configured source in the fixed goga
// precedence order — defaults, then files, then environment, then flags —
// regardless of the order the options that configured them were passed.
//
// This function is the entire reason goga/config exists, so it is worth being
// blunt about what it is not: it is not a sort of a list that was built in
// call order. There is no such list. An option writes into the field for its
// own kind, and the kinds are read here in a sequence written out by hand. For
// the order of two different kinds to change, somebody would have to edit
// these four loops — there is no input to [Load] that can do it.
//
// Within one kind the order is the order the options were passed, and that is
// meaningful: two [WithFile] calls mean the second file overrides the first,
// which is how a base file plus a per-environment overlay is expressed.
func (s *settings) sourcesInHouseOrder() []source {
	out := make([]source, 0, len(s.defaults)+len(s.files)+len(s.envs)+len(s.flagSets))

	for _, m := range s.defaults {
		out = append(out, defaultsSource(m))
	}
	for _, f := range s.files {
		out = append(out, f.source())
	}
	for _, e := range s.envs {
		out = append(out, e.source())
	}
	for _, fs := range s.flagSets {
		out = append(out, flagsSource(fs))
	}

	return out
}

// missingKeys returns the keys declared with [WithRequiredKeys] that k does not
// have, in declaration order.
func (s *settings) missingKeys(k *koanf.Koanf) []string {
	var missing []string
	for _, key := range s.requiredKeys {
		if !k.Exists(key) {
			missing = append(missing, key)
		}
	}
	return missing
}

// decodeHooks returns the hook chain [Load] decodes with: the caller's hooks
// first, then goga's two.
//
// The caller's come first so that a hook can pre-empt a built-in rather than
// only see its output. mapstructure composes hooks by feeding each one's result
// to the next, so a hook that turns a string into a time.Duration leaves
// nothing for [mapstructure.StringToTimeDurationHookFunc] to do; the reverse
// order would hand the caller's hook a duration and no way back to the string.
func (s *settings) decodeHooks() []mapstructure.DecodeHookFunc {
	hooks := make([]mapstructure.DecodeHookFunc, 0, len(s.hooks)+2)
	hooks = append(hooks, s.hooks...)
	return append(hooks,
		// Item 3.2: durations and slices decode out of the box. Without the
		// first, `timeout: 30s` is a decode error in every config file goga
		// reads; without the second, a comma-separated environment variable
		// decodes to a one-element slice containing the commas.
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	)
}

// WithDefaults supplies the lowest-precedence source: values used when nothing
// else sets them.
//
// The map may be nested (`{"database": {"max_conns": 10}}`) or flat with
// dotted keys (`{"database.max_conns": 10}`); both produce the same key paths.
//
// Passing it twice is allowed and the second map overrides the first, but a
// defaults map is not the place to express layering — that is what a second
// [WithFile] is for.
func WithDefaults(m map[string]any) Option {
	return func(s *settings) error {
		s.defaults = append(s.defaults, m)
		return nil
	}
}

// WithFile reads a configuration file, if it is there.
//
// A file that does not exist is NOT an error: the overwhelmingly common
// deployment has a config file in development and pure environment variables
// in production, and a loader that fails on the missing file forces every such
// project to branch around goga. Any other failure to read it — a permission
// error, a directory where a file was expected, a syntax error — IS an error,
// because those are misconfigurations rather than absences. Use
// [WithRequiredFile] when the file's absence is itself a misconfiguration.
//
// The format comes from the extension: .yaml, .yml, .json or .toml. An
// extension naming no parser is rejected here, at the call site that supplied
// it, rather than at the read.
func WithFile(path string) Option { return withFile(path, false) }

// WithRequiredFile reads a configuration file that must exist.
//
// Everything [WithFile] documents applies, except that the file's absence
// fails the load naming the path.
func WithRequiredFile(path string) Option { return withFile(path, true) }

// withFile is the body the two file options share.
func withFile(path string, required bool) Option {
	return func(s *settings) error {
		if path == "" {
			return errEmptyPath
		}
		ext := strings.ToLower(filepath.Ext(path))
		if _, ok := parsers[ext]; !ok {
			return &UnsupportedFormatError{
				Path:      path,
				Ext:       filepath.Ext(path),
				Supported: supportedExtensions(),
			}
		}
		s.files = append(s.files, fileSource{path: path, ext: ext, required: required})
		return nil
	}
}

// WithEnv reads environment variables whose name begins with prefix.
//
// # The convention
//
// One convention, stated here at the call site because prose elsewhere is not
// where a reader looks:
//
//   - the prefix is upper case, and is followed by the separator;
//   - "__" (two underscores) separates key-path segments;
//   - "_" (one underscore) is a literal underscore inside a segment;
//   - the whole name is lower-cased to form the key path.
//
// So, with prefix "GOGA":
//
//	GOGA__DATABASE__MAX_CONNS  ->  database.max_conns
//	GOGA__ADDR                 ->  addr
//
// The two-underscore separator is the part worth defending. The obvious
// alternative — one underscore for both jobs — cannot express max_conns at
// all: DATABASE_MAX_CONNS is database.max.conns, and every Go project that has
// tried it has ended up with a per-key exception table. Three projects in this
// workspace chose three different conventions before this one was written
// down; this is the one goga uses.
//
// prefix is normalised: trailing underscores are trimmed, so "GOGA", "GOGA_"
// and "GOGA__" all mean the same thing. An empty prefix is rejected — see
// [errEmptyEnvPrefix].
func WithEnv(prefix string) Option {
	return func(s *settings) error {
		trimmed := strings.TrimRight(prefix, "_")
		if trimmed == "" {
			return errEmptyEnvPrefix
		}
		s.envs = append(s.envs, envSource{prefix: trimmed + envSeparator})
		return nil
	}
}

// WithFlags reads a parsed pflag set: the highest-precedence source.
//
// # Flag names and key paths
//
// A flag's name is its key path, with "-" rewritten to "_" so that a flag and
// an environment variable name the same setting:
//
//	--database.max-conns  ->  database.max_conns  <-  GOGA__DATABASE__MAX_CONNS
//
// # Set flags and default flags
//
// A flag the user actually passed always wins. A flag left at its default
// counts only where no other source set that key, so a flag default behaves
// like a second defaults map rather than like a value the user typed. That
// distinction is the one thing about precedence that is genuinely subtle, and
// it is also the one that has been implemented backwards in this workspace
// before: pflag reports it through [pflag.Flag.Changed], and a key-rewriting
// callback that forgets to consult it turns every flag default into an
// override of the config file.
//
// The flag set should already be parsed. An unparsed set has no flag marked
// changed, so every flag in it behaves as a default.
func WithFlags(fs *pflag.FlagSet) Option {
	return func(s *settings) error {
		if fs == nil {
			return errNilFlagSet
		}
		s.flagSets = append(s.flagSets, fs)
		return nil
	}
}

// WithRequiredKeys declares keys that some source must set.
//
// A missing one fails the load with a [MissingKeysError] naming every key that
// is missing, not just the first: an operator who fixes one key and reruns to
// be told about the next is doing the loader's work for it.
//
// The keys are full paths in the same form [Config.Cut] and koanf take:
// "database.dsn", not "DATABASE__DSN".
func WithRequiredKeys(keys ...string) Option {
	return func(s *settings) error {
		for i, key := range keys {
			if key == "" {
				return errEmptyRequiredKey(i)
			}
		}
		s.requiredKeys = append(s.requiredKeys, keys...)
		return nil
	}
}

// errEmptyRequiredKey answers [WithRequiredKeys] given an empty key, naming its
// position: the list is usually a literal, and the position is what locates it.
func errEmptyRequiredKey(i int) error {
	return errors.New("goga/config: required key " + strconv.Itoa(i) + " must not be empty")
}

// WithDecodeHook adds a mapstructure decode hook.
//
// Durations and comma-separated slices already decode without one; see
// [settings.decodeHooks] for what goga installs and in what order. Reach for
// this when a field's type is the project's own — a net/url.URL, a log level,
// an enum with a String method.
//
// Hooks run in the order they are added, ahead of goga's built-ins, so a hook
// added here can pre-empt one of them.
func WithDecodeHook(h mapstructure.DecodeHookFunc) Option {
	return func(s *settings) error {
		if h == nil {
			return errNilDecodeHook
		}
		s.hooks = append(s.hooks, h)
		return nil
	}
}

// WithWatch reloads the configuration when any configured file changes, and
// calls fn with the result.
//
// Every source is re-read, in the same fixed order — not just the file that
// changed. Reloading only the changed file would let a file that lost every
// precedence contest at startup win one at run time, which is the same class of
// bug the fixed order exists to prevent.
//
// fn is called on the watcher's goroutine, once per change, with an [Event]
// carrying either the reloaded value or the error that stopped it. It must not
// block. A [Config] is never mutated by a reload: [Config.Value] is the value
// as of [Load], and the new one arrives in the event, so a reader of the old
// value never races a writer of the new one.
//
// Watching starts during [Load] and stops at [Config.Close]. Files that do not
// exist are not watched — there is nothing to watch — so a [WithFile] whose
// file appears later does not trigger a reload.
func WithWatch(fn func(Event)) Option {
	return func(s *settings) error {
		if fn == nil {
			return errNilWatch
		}
		s.watch = fn
		return nil
	}
}

// defaultsSource builds the source for one [WithDefaults] map.
func defaultsSource(m map[string]any) source {
	return source{
		kind: kindDefaults,
		name: "defaults",
		read: func(*koanf.Koanf) (map[string]any, error) {
			// Delim, not "": the map is allowed to be flat with dotted keys,
			// and confmap unflattens it. A map that is already nested has no
			// dotted keys for it to act on.
			return confmap.Provider(m, delim).Read()
		},
	}
}

// fileSource is one configured file.
type fileSource struct {
	path     string
	ext      string
	required bool
}

// source builds the pipeline source for f.
func (f fileSource) source() source {
	return source{kind: kindFile, name: "file:" + f.path, read: f.read}
}

// read parses the file, or returns a nil map when it is absent and optional.
func (f fileSource) read(*koanf.Koanf) (map[string]any, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		if !f.required && errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	// The extension was validated by the option, so the lookup cannot miss.
	mp, err := parsers[f.ext]().Unmarshal(b)
	if err != nil {
		return nil, err
	}
	return mp, nil
}

// envSource is one configured environment prefix, already normalised to end
// with [envSeparator].
type envSource struct {
	prefix string
	// environ overrides os.Environ. It is nil outside this package's own
	// tests: no exported option sets it, because injecting an environment is
	// something only a test of the transform itself needs.
	environ func() []string
}

// source builds the pipeline source for e.
func (e envSource) source() source {
	name := "env:" + e.prefix
	return source{
		kind: kindEnv,
		name: name,
		read: func(*koanf.Koanf) (map[string]any, error) {
			return env.Provider(delim, env.Opt{
				Prefix:        e.prefix,
				TransformFunc: e.transform,
				EnvironFunc:   e.environ,
			}).Read()
		},
	}
}

// transform maps one environment variable name to a key path, per the
// convention [WithEnv] documents. Returning "" drops the variable.
func (e envSource) transform(key, value string) (string, any) {
	rest, ok := strings.CutPrefix(key, e.prefix)
	if !ok || rest == "" {
		return "", nil
	}
	return strings.ToLower(strings.ReplaceAll(rest, envSeparator, delim)), value
}

// flagsSource builds the source for one [WithFlags] flag set.
func flagsSource(fs *pflag.FlagSet) source {
	return source{
		kind: kindFlags,
		name: "flags",
		read: func(k *koanf.Koanf) (map[string]any, error) {
			return posflag.ProviderWithFlag(fs, delim, k, flagKey(fs)).Read()
		},
	}
}

// flagKey rewrites a flag name into a key path, matching the env convention:
// "-" between words becomes "_", and "." keeps separating path segments.
//
// The callback is only ever the key-and-value rewrite. posflag applies the
// [pflag.Flag.Changed] test AFTER calling it, against the key the callback
// returned, so an unchanged flag still cannot override a source that already
// set that key. Verified against posflag's source rather than assumed: an
// earlier posflag release ran the callback in place of that test, and a
// callback written for it inverts the precedence of every flag default.
func flagKey(fs *pflag.FlagSet) func(*pflag.Flag) (string, any) {
	return func(f *pflag.Flag) (string, any) {
		key := strings.ReplaceAll(f.Name, flagSeparator, flagWordSeparator)
		return key, posflag.FlagVal(fs, f)
	}
}
