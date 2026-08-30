// Package registry maps a plain adapter name — the string that appears in a
// config file — onto a constructor for a port, and back again.
//
// A goga module declares a port as an interface and ships one or more adapters
// implementing it. Which adapter a program uses is a deployment decision, so it
// arrives as configuration: a name plus a subtree of raw settings. The registry
// is the seam where that untyped pair becomes a typed value.
//
// There are two ways through it.
//
// The config-driven path is [Registry.Open]: given a name and a [Settings]
// subtree, it decodes the settings into the adapter's own settings type and
// calls its constructor. The port must be instantiated explicitly, because it
// appears only in the result, and the registry checks it against the port the
// adapter was registered for before constructing anything.
//
// The typed path is [Adapter], the handle returned by [Registry.Provide]. Both
// its type parameters are fixed by the handle, so a caller writes no type
// arguments at all and the compiler rejects an option belonging to a different
// adapter. [Adapter.Open] applies raw configuration first and the caller's
// options on top.
//
// # Imports
//
// This package imports the standard library and the root goga package, and
// nothing else. The single goga import exists so that [Option] can be a type
// *alias* of [goga.Option]. It must be an alias and not a second identical
// declaration: two identical declarations are distinct named types, so an
// adapter's option would not be usable with [goga.Apply] and vice versa.
//
// The registry deliberately carries no Instrumentation and no telemetry import.
// Telemetry is itself a module with adapters, so it will want to reach the
// registry; if the registry reached back, registry → telemetry → registry would
// be an import cycle. A caller that wants an opened adapter instrumented wraps
// the value it gets back.
package registry

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/gaarutyunov/goga"
)

// Settings is one adapter's raw configuration subtree, as it comes out of a
// config file before anything knows which adapter it belongs to.
type Settings map[string]any

// Option is a generic type alias of [goga.Option].
//
// It is an alias and not a declaration on purpose. Two identical type
// declarations are distinct named types in Go, so a `type Option[S any]
// func(*S) error` here would produce options that [goga.Apply] could not fold
// and options from goga that [Adapter.Open] could not accept. The alias makes
// them the same type.
type Option[S any] = goga.Option[S]

// Decode is the injected seam between the registry and a configuration
// library. goga/config supplies it; the registry never imports one.
//
// A decoder SHOULD reject unknown keys, so that a mistyped setting is a startup
// error rather than a silently zero value. dst is always a non-nil pointer to
// an adapter's settings struct.
//
// The registry calls a decoder only when there is something to decode: an Open
// with empty or nil raw settings skips it, which is what makes the
// options-only path on [Adapter.Open] work without a config file.
type Decode func(raw Settings, dst any) error

// Sentinels for errors.Is. Each has a corresponding typed error carrying the
// details, reachable with errors.As.
var (
	// ErrDuplicateName reports a name registered twice.
	ErrDuplicateName = errors.New("goga/registry: duplicate adapter name")
	// ErrUnknownName reports a name that was never registered.
	ErrUnknownName = errors.New("goga/registry: unknown adapter name")
	// ErrPortMismatch reports an adapter retrieved as a port it was not
	// registered for.
	ErrPortMismatch = errors.New("goga/registry: adapter port mismatch")
)

// DuplicateNameError is returned by [Registry.Register] and [Registry.Provide]
// when a name is already taken. It is an error rather than a panic because the
// registry is an injected value with no init() behind it: a duplicate surfaces
// while ordinary startup code is running, where an error can be reported.
type DuplicateNameError struct {
	// Name is the adapter name that was already taken.
	Name string
	// Port is the port the existing registration was made for.
	Port reflect.Type
}

func (e *DuplicateNameError) Error() string {
	return fmt.Sprintf("goga/registry: register %q: name already registered for port %s", e.Name, typeName(e.Port))
}

// Is reports whether target is [ErrDuplicateName], so that a caller can branch
// on the condition with errors.Is without naming this type, and reach for the
// type with errors.As only when it wants the [DuplicateNameError.Name] and
// [DuplicateNameError.Port] fields.
func (e *DuplicateNameError) Is(target error) bool { return target == ErrDuplicateName }

// UnknownNameError is returned when no adapter is registered under the
// requested name. It lists what is registered, because the overwhelmingly
// likely cause is a typo in a config file.
type UnknownNameError struct {
	// Name is the name that was asked for.
	Name string
	// Port is the port it was asked for.
	Port reflect.Type
	// Registered holds every name currently in the registry, sorted.
	Registered []string
}

func (e *UnknownNameError) Error() string {
	known := "registry is empty"
	if len(e.Registered) > 0 {
		known = "registered: " + strings.Join(e.Registered, ", ")
	}
	return fmt.Sprintf("goga/registry: no adapter %q registered for port %s (%s)", e.Name, typeName(e.Port), known)
}

// Is reports whether target is [ErrUnknownName], so that a caller can branch on
// the condition with errors.Is without naming this type, and reach for the type
// with errors.As only when it wants [UnknownNameError.Registered] — the list of
// names that were available, which is what turns "unknown adapter" into a
// message a user can act on.
func (e *UnknownNameError) Is(target error) bool { return target == ErrUnknownName }

// PortMismatchError is returned when an adapter exists under the requested name
// but was registered for a different port.
//
// The check is by type identity, not by method set. An adapter registered for
// one interface therefore cannot be retrieved as an unrelated interface that
// happens to have the same methods — which is exactly the confusion a
// name-keyed registry would otherwise invite.
type PortMismatchError struct {
	// Name is the adapter name.
	Name string
	// Registered is the port the adapter was registered for.
	Registered reflect.Type
	// Requested is the port the caller asked for.
	Requested reflect.Type
}

func (e *PortMismatchError) Error() string {
	return fmt.Sprintf("goga/registry: adapter %q is registered for port %s, not %s",
		e.Name, typeName(e.Registered), typeName(e.Requested))
}

// Is reports whether target is [ErrPortMismatch], so that a caller can branch on
// the condition with errors.Is without naming this type, and reach for the type
// with errors.As only when it wants to report the two ports involved —
// [PortMismatchError.Registered] and [PortMismatchError.Requested].
func (e *PortMismatchError) Is(target error) bool { return target == ErrPortMismatch }

// typeName renders a port for an error message. reflect.Type is used rather
// than %T on a zero value because %T on a nil interface prints "<nil>", which
// names nothing.
func typeName(t reflect.Type) string {
	if t == nil {
		return "<unknown>"
	}
	return t.String()
}

// entry is one registration. port and settings are recorded so that a
// retrieval can be checked before any constructor runs; open closes over the
// constructor with both of its type parameters still statically known.
type entry struct {
	port     reflect.Type
	settings reflect.Type
	ctor     any // func(context.Context, S) (P, error)

	// open decodes raw into a fresh S, calls the constructor, and stores the
	// resulting P through sink, which is always a *P. Boxing the result in an
	// any instead would lose a constructor's legitimate nil interface value.
	open func(ctx context.Context, dec Decode, raw Settings, sink any) error
}

// Registry maps adapter names to constructors. The zero value is not usable;
// call [New].
//
// A Registry is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	decode  Decode
	entries map[string]entry
}

// New returns an empty registry that decodes adapter settings with decode.
//
// It panics if decode is nil. A registry with no decoder cannot open anything
// that has settings, and there is no sensible recovery from that wiring
// mistake — it is a programming error, not a runtime condition.
func New(decode Decode) *Registry {
	if decode == nil {
		panic("goga/registry: New called with a nil Decode")
	}
	return &Registry{decode: decode, entries: make(map[string]entry)}
}

// Register records ctor under a plain adapter name, for the port P it returns
// and the settings type S it takes.
//
// Both type parameters are inferred from ctor, so no caller ever writes S.
// That is what lets an adapter keep its settings struct unexported while still
// being registered from another package.
//
// It returns a *[DuplicateNameError] (matching [ErrDuplicateName]) if the name
// is taken.
func (r *Registry) Register[P any, S any](name string, ctor func(context.Context, S) (P, error)) error {
	if ctor == nil {
		return fmt.Errorf("goga/registry: register %q: nil constructor", name)
	}

	e := entry{
		port:     reflect.TypeFor[P](),
		settings: reflect.TypeFor[S](),
		ctor:     ctor,
		open: func(ctx context.Context, dec Decode, raw Settings, sink any) error {
			s, err := decodeSettings[S](dec, raw)
			if err != nil {
				return err
			}
			p, err := ctor(ctx, s)
			if err != nil {
				return err
			}
			// sink is a *P whenever [Registry.Open] has compared its own P
			// against e.port first, which is the only path that reaches here.
			// The check costs two lines and turns a future violation of that
			// invariant into a wrapped error naming both types, rather than a
			// panic from inside a closure with no name on it.
			out, ok := sink.(*P)
			if !ok {
				return fmt.Errorf("result sink is %s, want %s: the port check before open was skipped",
					typeName(reflect.TypeOf(sink)), typeName(reflect.TypeFor[*P]()))
			}
			*out = p
			return nil
		},
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.entries[name]; ok {
		return &DuplicateNameError{Name: name, Port: prev.port}
	}
	r.entries[name] = e
	return nil
}

// Open is the config-driven path: it looks up name, checks that the adapter was
// registered for port P, decodes raw into the adapter's own settings type, and
// calls its constructor.
//
// P is result-only, so it must be instantiated explicitly:
//
//	tr, err := reg.Open[mcp.Transport](ctx, cfg.Transport, cfg.Settings)
//
// The port check happens before the constructor runs, so an adapter registered
// for one port can never be handed back as an unrelated interface that happens
// to share its method set.
func (r *Registry) Open[P any](ctx context.Context, name string, raw Settings) (P, error) {
	var zero P

	e, ok := r.lookup(name)
	if !ok {
		return zero, &UnknownNameError{Name: name, Port: reflect.TypeFor[P](), Registered: r.Names()}
	}
	if want := reflect.TypeFor[P](); e.port != want {
		return zero, &PortMismatchError{Name: name, Registered: e.port, Requested: want}
	}

	var out P
	if err := e.open(ctx, r.decode, raw, &out); err != nil {
		return zero, fmt.Errorf("goga/registry: open %q: %w", name, err)
	}
	return out, nil
}

// Names returns every registered adapter name, sorted.
//
// It takes the read lock. That looks like an excess of caution for a read, and
// is not: without it, -race reports a data race against a concurrent
// [Registry.Register] on the first test that does both.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Provide registers ctor and returns a typed handle for it in one call. It
// returns [Registry.Register]'s duplicate-name error rather than swallowing it;
// the returned handle is unusable when the error is non-nil.
func (r *Registry) Provide[P any, S any](name string, ctor func(context.Context, S) (P, error)) (Adapter[P, S], error) {
	if err := r.Register(name, ctor); err != nil {
		return Adapter[P, S]{}, err
	}
	return Adapter[P, S]{name: name, reg: r}, nil
}

// lookup returns the entry for name under the read lock.
func (r *Registry) lookup(name string) (entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	return e, ok
}

// Adapter is a typed handle onto one registration.
//
// Both of its type parameters belong to the type, not to its methods, so they
// are already fixed by the time a caller has a handle: [Adapter.Open] takes no
// type arguments, and an option built for a different adapter's settings is a
// compile error rather than a runtime one.
//
// A handle is nameable downstream without naming the settings type, which is
// what lets that type stay unexported:
//
//	// in package pgx, where settings is unexported
//	type Adapter = registry.Adapter[*pgxpool.Pool, settings]
//
// The zero Adapter is not usable; obtain one from [Registry.Provide].
type Adapter[P any, S any] struct {
	name string
	reg  *Registry
}

// Name returns the adapter name the handle was registered under.
func (a Adapter[P, S]) Name() string { return a.name }

// Open decodes raw into the adapter's settings and then applies opts on top.
//
// The precedence is deliberate. Raw configuration is the general form and
// options are the explicit, more specific one, so an option written at the call
// site wins over whatever the config file said. Passing a nil raw makes this a
// pure options path, with no decoder involved at all.
//
// An option that returns an error is reported as "goga: applying option: %w" —
// the shape [goga.Apply] uses, because it is [goga.Apply] that produces it — so
// a bad option reads the same whichever path it came through.
func (a Adapter[P, S]) Open(ctx context.Context, raw Settings, opts ...Option[S]) (P, error) {
	var zero P

	if a.reg == nil {
		return zero, fmt.Errorf("goga/registry: open %q: adapter handle has no registry (zero value)", a.name)
	}

	e, ok := a.reg.lookup(a.name)
	if !ok {
		return zero, &UnknownNameError{Name: a.name, Port: reflect.TypeFor[P](), Registered: a.reg.Names()}
	}
	if want := reflect.TypeFor[P](); e.port != want {
		return zero, &PortMismatchError{Name: a.name, Registered: e.port, Requested: want}
	}
	ctor, ok := e.ctor.(func(context.Context, S) (P, error))
	if !ok {
		return zero, fmt.Errorf("goga/registry: open %q: handle expects settings %s, adapter was registered with %s",
			a.name, typeName(reflect.TypeFor[S]()), typeName(e.settings))
	}

	s, err := decodeSettings[S](a.reg.decode, raw)
	if err != nil {
		return zero, fmt.Errorf("goga/registry: open %q: %w", a.name, err)
	}
	if s, err = goga.Apply(s, opts...); err != nil {
		return zero, err
	}

	p, err := ctor(ctx, s)
	if err != nil {
		return zero, fmt.Errorf("goga/registry: open %q: %w", a.name, err)
	}
	return p, nil
}

// decodeSettings produces one adapter's settings value from a raw subtree. An
// empty subtree skips the decoder entirely and yields the zero S, which is what
// makes an options-only Open work without any configuration present.
func decodeSettings[S any](dec Decode, raw Settings) (S, error) {
	var s S
	if len(raw) == 0 {
		return s, nil
	}
	if err := dec(raw, &s); err != nil {
		return s, fmt.Errorf("decoding settings into %s: %w", typeName(reflect.TypeFor[S]()), err)
	}
	return s, nil
}
