package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The sentinels a caller branches on with [errors.Is]. Each of the typed
// errors below answers one of them, so a caller that only wants to know what
// went wrong never has to name the type, and reaches for [errors.As] only when
// it wants the fields.
var (
	// ErrMissingKeys is the condition [MissingKeysError] reports: one or more
	// keys declared with [WithRequiredKeys] were not set by any source.
	ErrMissingKeys = errors.New("goga/config: required key not set")

	// ErrCollidingKeys is the condition [CollidingKeysError] reports: one
	// source set a key as a scalar that another source uses as the parent of a
	// nested key.
	ErrCollidingKeys = errors.New("goga/config: colliding keys")

	// ErrUnsupportedFormat is the condition [UnsupportedFormatError] reports:
	// a file whose extension names no parser goga/config knows.
	ErrUnsupportedFormat = errors.New("goga/config: unsupported file format")
)

// errEmptyEnvPrefix answers [WithEnv] called with a prefix that is empty once
// its trailing underscores are trimmed.
//
// An empty prefix is rejected rather than treated as "read every variable",
// because a loader that reads every variable in the environment merges PATH,
// HOME and whatever the CI runner exports into the application's
// configuration, and does it silently.
var errEmptyEnvPrefix = errors.New("goga/config: env prefix must not be empty")

// errNilFlagSet answers [WithFlags] called with a nil flag set.
var errNilFlagSet = errors.New("goga/config: flag set must not be nil")

// errNilWatch answers [WithWatch] called with a nil callback.
var errNilWatch = errors.New("goga/config: watch callback must not be nil")

// errNilDecodeHook answers [WithDecodeHook] called with a nil hook.
var errNilDecodeHook = errors.New("goga/config: decode hook must not be nil")

// errEmptyPath answers [WithFile] and [WithRequiredFile] called with "".
var errEmptyPath = errors.New("goga/config: file path must not be empty")

// MissingKeysError reports the keys declared with [WithRequiredKeys] that no
// source supplied.
//
// It carries the keys rather than only naming them in its message because the
// caller that can act on them is usually not a human reading a log line: a
// composition root reporting which settings an operator still has to provide
// wants the list, not a sentence it has to parse back apart.
type MissingKeysError struct {
	// Keys are the missing keys, in the order they were declared.
	Keys []string
}

// Error implements error, listing every missing key rather than only the first.
// An operator who fixes one key and reruns to be told about the next is being
// made to do the loader's work.
func (e *MissingKeysError) Error() string {
	quoted := make([]string, 0, len(e.Keys))
	for _, key := range e.Keys {
		quoted = append(quoted, strconv.Quote(key))
	}
	return "goga/config: required " + plural("key", len(e.Keys)) + " not set: " +
		strings.Join(quoted, ", ")
}

// Is reports whether target is [ErrMissingKeys], so that a caller can branch on
// the condition without naming this type and use [errors.As] only when it wants
// [MissingKeysError.Keys].
func (e *MissingKeysError) Is(target error) bool { return target == ErrMissingKeys }

// CollidingKeysError reports the one shape koanf cannot represent: a key held
// as a scalar by one source and used as the parent of a nested key by another.
//
// koanf has no error for this. Whichever source merges last wins, and the other
// key — the whole subtree, if the scalar wins — is gone, with nothing recorded
// anywhere. The usual way in is a feature switch: `catalog: true` in a defaults
// map beside `catalog.base_path` in a file, or a `CATALOG` environment variable
// beside a `catalog.dsn` in YAML. It bites credentials hardest, because a
// credential is exactly the value an operator overrides from the environment.
//
// The rule the collision teaches is worth stating once: a switch is
// `catalog.enabled`, never `catalog`. A key that has children can never also
// have a value.
type CollidingKeysError struct {
	// Scalar is the key one source set to a value.
	Scalar string
	// Nested is a key another source set below it. Where several exist, this
	// is the first in sorted order — one example is enough to show the shape.
	Nested string
	// ScalarSource and NestedSource name the two sources involved, in the same
	// vocabulary as the goga.config.sources span attribute: "defaults",
	// "file:<path>", "env:<prefix>", "flags".
	ScalarSource string
	NestedSource string
}

// Error implements error, naming both keys and both sources: the fix requires
// knowing which file or prefix to edit, and the keys alone do not say.
func (e *CollidingKeysError) Error() string {
	return fmt.Sprintf(
		"goga/config: %s sets %s as a value while %s sets %s below it; "+
			"a key with children cannot also have a value, so one of them would be "+
			"silently discarded (make the scalar %s instead)",
		e.ScalarSource, strconv.Quote(e.Scalar),
		e.NestedSource, strconv.Quote(e.Nested),
		strconv.Quote(e.Scalar+delim+"enabled"))
}

// Is reports whether target is [ErrCollidingKeys], so that a caller can branch
// on the condition without naming this type.
func (e *CollidingKeysError) Is(target error) bool { return target == ErrCollidingKeys }

// UnsupportedFormatError reports a configuration file whose extension names no
// parser goga/config knows.
//
// It is returned by [WithFile] and [WithRequiredFile] rather than by [Load], so
// that a path typed wrong fails at the call site that supplied it instead of at
// the first read.
type UnsupportedFormatError struct {
	// Path is the file that was configured.
	Path string
	// Ext is its extension, including the leading dot, or "" when it has none.
	Ext string
	// Supported lists the extensions goga/config does parse, sorted.
	Supported []string
}

// Error implements error, listing the extensions that would have worked. The
// overwhelmingly likely cause is a ".conf" or a ".cfg" written out of habit,
// and the list is the answer to the question that follows.
func (e *UnsupportedFormatError) Error() string {
	ext := e.Ext
	if ext == "" {
		ext = "no extension"
	} else {
		ext = strconv.Quote(ext)
	}
	return fmt.Sprintf("goga/config: file %s has %s; supported: %s",
		strconv.Quote(e.Path), ext, strings.Join(e.Supported, ", "))
}

// Is reports whether target is [ErrUnsupportedFormat], so that a caller can
// branch on the condition without naming this type.
func (e *UnsupportedFormatError) Is(target error) bool { return target == ErrUnsupportedFormat }

// plural renders a count-appropriate noun for an error message.
func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}
