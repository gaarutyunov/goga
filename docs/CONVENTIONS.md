# goga conventions

The rules that hold across every goga module, written down once here and
applied by every milestone after M0. They are not style preferences: each of
the five cross-cutting rules in the first section is a defect the spec review
found in the design's *own* pseudocode, which is the evidence that stating a
rule once is cheaper than catching it fifteen times.

Every rule here is enforced — by the compiler, by `goga/lint`, or by CI. A
convention with no mechanism is a goga defect, not a caveat (design D5).

Source: `openspec/changes/goga-issue-1-framework-foundations/design.md` in
`gaarutyunov/workspace`, decisions D5, D14, D15, D17, D19, D20 and D22.

---

## 1. Cross-cutting Go conventions (D15)

### 1.1 A method that opens a span uses named result parameters

The house shape is `defer func() { end(err) }()`, and the deferred closure
observes the *variable* `err` — not the value a `return` expression computed.

```go
func (s *Server) Shutdown(ctx context.Context) (err error) {
	ctx, end := s.instr.Start(ctx, "serve.Shutdown")
	defer func() { end(err) }()

	if err = s.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("goga/serve: shutdown: %w", err)
	}
	return nil
}
```

**Why.** With unnamed results, `return nil, &UnknownSchemeError{…}` never
assigns the local `err`, so the deferred call records `nil` — success — on
every failure. That inverts the one signal telemetry exists to produce, and it
does so silently. The design's own pseudocode had it. Every goga method that
opens a span declares named results.

### 1.2 `Instrumentation.Start` returns the closer

```go
// goga/telemetry
func (i *Instrumentation) Start(
	ctx context.Context, op string, attrs ...attribute.KeyValue,
) (ctx2 context.Context, end func(error))
```

Never a `(ctx, span)` pair that the caller passes back to an
`End(ctx, span, err, start)` along with a start time it captured itself.

**Why.** The three-argument form was already mis-called in the design that
introduced it: `migrate.Up`'s inner loop passed `time.Now()` as the *start*,
recording a duration of zero for every migration — the exact metric the
migration module exists to produce. An API where the duration is capturable
only by the type that started it cannot be mis-called that way. The span stays
reachable through `trace.SpanFromContext(ctx)` for a caller that wants to add
attributes mid-operation.

### 1.3 A method returning a streaming result never cancels that result's context

Cancellation is owned by whatever outlives the call. The returned value owns
both the cancel and the span, and ends both in its `Close`.

```go
func (c *Client) Stream(ctx context.Context) (_ *Stream, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	ctx, end := c.instr.Start(ctx, "client.Stream")
	// No defer cancel() and no defer end(err): on success the returned
	// *Stream owns both. On the error path they are released here.
	rows, err := c.open(ctx)
	if err != nil {
		end(err)
		cancel()
		return nil, fmt.Errorf("goga/client: stream: %w", err)
	}
	return &Stream{rows: rows, end: end, cancel: cancel}, nil
}

// Close ends the span and cancels the context. Closing twice is harmless.
func (s *Stream) Close() error { … }
```

**Why.** A `defer cancel()` in such a method kills the stream before the caller
reads the first item, and a `defer end(err)` closes the span before the
operation has done any work — the recorded duration covers the setup rather
than the read. The defect was found in a `database.Query` that returned `Rows`
and cancelled them on the way out; that method has since been removed with its
port, but the rule is not tied to it. It applies to every goga API that returns
something the caller reads incrementally.

**Non-streaming methods keep the defer.** `Exec`, `Up`, `CallTool` and their
kind do all their work before returning, so 1.1's shape is correct for them.

`goga/lint`'s `gogastream` flags a `defer cancel()` in a function returning an
interface with a `Close`.

### 1.4 One process, one signal handler

Signal handling belongs to the composition root — `cli.App.Run`, and `app.Run`
beneath it — and nowhere else.

```go
// goga/cli, and only here.
ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
defer stop()

// Every surface: takes a ctx, returns when it is cancelled.
func (s *Server) Run(ctx context.Context) (err error)
```

**Why.** A process serving HTTP and MCP together — a case goga explicitly
supports — would otherwise install three handlers whose shutdowns race, with no
ordering between draining connections, closing the pool and flushing
telemetry. The requirement that *every surface stops together* is then
unimplementable. `app.Run` runs the surfaces under an `errgroup` and, on
cancellation, shuts down in reverse construction order: surfaces drain, then
the database, then telemetry flushes last so the shutdown itself is observable.

The single handler ships with `goga/cli` (M8). A project adopting an earlier
module keeps its own signal handling until then, and goga's `Run` simply takes
the context it is given.

### 1.5 Errors carry the module path, and are typed where callers branch

```go
// goga/database
type UnknownSchemeError struct{ Scheme string }

func (e *UnknownSchemeError) Error() string {
	return "unknown scheme " + strconv.Quote(e.Scheme)
}

// Is lets a caller branch with errors.Is as well as errors.As.
func (e *UnknownSchemeError) Is(target error) bool {
	_, ok := target.(*UnknownSchemeError)
	return ok
}

func Open(ctx context.Context, dsn string) (db *DB, err error) {
	…
	return nil, fmt.Errorf("goga/database: open %q: %w",
		dsn, &UnknownSchemeError{Scheme: scheme})
}
```

The shape is `fmt.Errorf("goga/<module>: <op>: %w", err)` — module prefix,
operation, `%w`. A caller that must branch gets a type with an `Is`, not a
string to match on.

**Why.** Without the prefix an error surfacing three layers up names no origin;
without `%w` a caller cannot unwrap to the driver error underneath. And
**adapters return errors and never log**: the portable type owns both the log
and the span, so an adapter that logs produces a duplicate record with none of
the span's context attached.

---

## 2. The house settings shape (D5, D14)

Variadic functional options everywhere; no parameter structs in the
caller-facing surface. The rule is stated **by side of the port**, because the
two sides have different audiences and different needs.

### 2.1 Caller-facing: an unexported `settings`, and an exported `Option` alias

```go
package serve

// settings is UNEXPORTED, so no other package can name it, construct it or
// embed it.
type settings struct {
	readHeaderTimeout time.Duration
	shutdownGrace     time.Duration
}

// Option is an exported alias over that unexported type: a caller can hold and
// pass a serve.Option and cannot write the type it mutates.
type Option = goga.Option[settings]

func WithReadHeaderTimeout(d time.Duration) Option {
	return func(s *settings) error {
		if d <= 0 {
			return fmt.Errorf(
				"goga/serve: read header timeout must be > 0, got %s", d)
		}
		s.readHeaderTimeout = d
		return nil
	}
}
```

**No goga entry point takes a settings value.** Every exported constructor
takes `...Option`, so `goga.Apply` over the caller's options is the only way a
populated `settings` ever comes into existence. That is what makes "no
parameter structs" a compile-time property rather than a review one.

Naming: `With<Noun>` sets, `With<Noun>s(...T)` appends, `Without<Noun>`
removes. `WithoutTelemetry` does not exist, by decision — telemetry is an
invariant, not a feature.

The same holds for an **adapter's** own settings: an adapter's `S` is inferred
from the constructor it registers, so no caller ever names it and the adapter
keeps `settings` unexported too.

### 2.2 Driver-facing: an exported `Options` struct in the `driver` package

```go
package driver // goga/serve/driver

// Options is what a port hands an adapter. It is exported by necessity.
type Options struct {
	ReadHeaderTimeout time.Duration
	ShutdownGrace     time.Duration
}
```

```go
// serve copies the fields across at construction.
func (s *settings) driverOptions() driver.Options {
	return driver.Options{ReadHeaderTimeout: s.readHeaderTimeout /* … */}
}
```

**Why it is exported, and why that is not an exception.** An adapter in another
package has to name `driver.Options` in its method signatures to implement the
port at all, and the conformance suite (§3.4) lives in a third package and has
to construct one. Constructing one buys a caller nothing: there is no goga
entry point that accepts one, and the only way to obtain the portable type is
the module's own constructor, which instruments. The driver-side struct is the
vocabulary of a boundary the application never reaches, not an alternative
entry point.

**The two types do not alias and are allowed to diverge.** Duplicate the
fields; do not embed one in the other. That is gocloud.dev's rule, and the
reason is that each type's godoc addresses its own audience. New
`driver.Options` fields are **additive**, and an adapter may ignore any field
it does not support.

| side | shape | visibility |
|---|---|---|
| caller-facing (module and adapter settings) | variadic `Option[S]` over an unexported struct | **unexported** |
| driver-facing (per-call options a port hands an adapter) | plain struct, additive fields only | **exported** |

### 2.3 An adapter exports its handle by an alias, not by exporting `settings`

```go
package pgxdb // goga/database/pgxdb

type settings struct {
	DSN      string `koanf:"dsn"`
	MaxConns int    `koanf:"max_conns"`
}

// Every adapter package declares this alias.
type PgxAdapter = registry.Adapter[*pgxpool.Pool, settings]

func Provide(r *registry.Registry) (PgxAdapter, error) {
	return r.Provide("pgx", newPool)
}
```

**The failure it fixes.** Without the alias the typed handle is unusable under
the DI rules: a downstream wire provider that takes it as a parameter, or a
composition root that holds it as a field, cannot *name* the type — `settings`
is unexported, and the compiler says *"name settings not exported by package
pgxdb"*. The handle would work only as a local `:=` inside one function, so
`wire.NewSet(Provide)` would contribute a node nothing could depend on. The
alias name is exported, the type argument is not, and a consumer names the
alias — verified as a provider parameter, a struct field and a second
provider's input.

**This is not the version-gated alias feature.** `PgxAdapter` aliases an
*already-instantiated* generic type and compiles at Go 1.22. Only an alias
carrying its own type parameters — `type Option[S any] = goga.Option[S]` —
needs Go 1.23. Neither is what sets goga's floor (§4).

---

## 3. Package boundaries (D19, D20, D22)

The conclusions of a read of `gocloud.dev` at commit `35f55f24`, applied.

### 3.1 One module, one package per module, one package per adapter

goga is **one** Go module, `github.com/gaarutyunov/goga`. Not one module per
adapter — the release tax of the multi-module layout is real and measurable,
and Go's module graph pruning already gives the property that matters:

**A project that does not import an adapter does not compile it and does not
carry it in its build list.** Measured on go-cloud: a consumer importing only
`gocloud.dev/blob` plus the in-memory driver ends up with 19 indirect requires
and zero AWS or Azure SDK. That is why adapters are sub-packages of their
module (`database/pgxdb`, `mcp/httptransport`) rather than separate modules.

Three rules keep it true, and the first two are enforced rather than
documented:

1. **No shared or internal goga package may import anything heavier than
   OpenTelemetry and the standard library.** go-cloud's errors package drags in
   gRPC and its retry helper drags in `gax-go/v2`. If goga ever needs gRPC
   status codes, that mapping lives in `goga/grpc` — never in a shared package.
   `depguard` limits `goga/telemetry` and `goga/registry` accordingly, and a CI
   job builds a throwaway module importing one module plus one adapter, runs
   `go mod tidy`, and fails on any require outside an allowlist.
2. **No `_test.go` file in a portable package may import an adapter.**
   `gocloud.dev/blob/example_test.go` imports `gcsblob` and `s3blob`, and
   because `go mod tidy` follows the test dependencies of imported packages,
   `cloud.google.com/go/storage` lands in every consumer's `go.mod`. The leak
   is invisible until somebody else runs `go mod tidy`. Adapter examples live
   in the adapter's own package; `goga/lint`'s `gogatestimport` enforces it.
3. **Split a module out only for a genuinely toxic dependency** — cgo, a
   conflicting transitive version, or an SDK on the scale go-cloud split out
   for mongo, kafka and nats.

goga's own layout is **flat**: no `pkg/`, no `internal/` for goga's own code.
`goga/lint`'s `gogalayout` runs on goga itself.

### 3.2 `driver` packages are exempt from the v1 freeze, and say so

What v1 freezes: every portable type and its exported methods, the root `goga`
package (`Option`, `Apply`), `goga/registry`'s exported surface, and the `As`
contract.

What is **not** frozen: `goga/*/driver` packages. They are the extension point
and they evolve, by two channels — additive option-struct fields, and new
*optional* interfaces. Adding a method to an existing driver interface is a
breaking change and needs a major version. **Each `driver` package states its
exemption in its package doc**; the promise is worthless where the reader
cannot see it.

goga's adapters are in-tree, which is what makes this affordable — the same
reason it is affordable for go-cloud. The day an out-of-tree adapter exists is
the day the `driver` packages need a compatibility promise of their own.

### 3.3 `As` is the single downcast shape

```go
// As converts i to an adapter-specific type. It returns false if the adapter
// does not support the requested type. Callers must degrade gracefully.
//
// As is a runtime assertion: the compiler does not know the dynamic type
// behind the port.
func (s *Server) As(i any) bool
```

The adapter's implementation is the whole of it:

```go
func (s *server) As(i any) bool {
	p, ok := i.(**gin.Engine)
	if !ok {
		return false
	}
	*p = s.engine
	return true
}
```

Three rules:

- **`As` returning false is not an error.** Callers skip the adapter-specific
  tweak and carry on, so the same code still runs against the in-memory or test
  adapter. A caller that errors on `false` has written adapter-locked code
  without saying so.
- **Every adapter documents what it supports**, in an `# As` section in its
  package doc — including *"this adapter supports no types for `As`"*.
- **goga does not adopt go-cloud's `BeforeX(asFunc)` callbacks.** "Reach the
  underlying object" is affordable; "mutate every request in flight" is a new
  decision if a case for it ever appears.

**Why an escape hatch exists at all in a framework whose point is that projects
do not touch the tool:** the alternative is not purity, it is the port growing
a leaky union of every adapter's surface. Without a hatch, the first project
that needs a middleware goga does not expose either forks goga or drops it.

**`Get[A any]` was dropped, and the reason is instructive.** It was an
unconstrained downcast — `A` cannot be constrained to `P` — so it compiled for
any `A` and failed at run time. `As` is honest about being the same runtime
assertion; a generic signature that merely *looks* static is worse than one
that admits what it is.

### 3.4 Conformance suites, where a port has more than one implementation

Where a port has real alternatives, adapters must be interchangeable, and
"interchangeable" is a claim only a shared test suite can make. The suite lives
in `<module>test` (`servetest`, `migratetest`), an adapter opts in with roughly
thirty lines, and the suite injects its own invariants regardless of what the
adapter opted into — every adapter is checked for `As(nil) == false` whether it
asked or not.

A port with one implementation deliberately gets **no** suite. A conformance
suite for one implementation is pure cost.

---

## 4. The Go 1.27 floor

`go.mod` says `go 1.27`, with **no `toolchain` directive**. Go 1.27.0 is GA, so
`GOTOOLCHAIN=auto` resolves it; pinning a release candidate would drag every
consumer onto a pre-release toolchain for no reason.

**What sets the floor.** `goga/registry`'s four exported forms are **generic
methods on `*Registry`**, on the owner's decision. Nothing else in goga needs
anything newer than Go 1.22 (`reflect.TypeFor`) or 1.23 (the generic type
alias). That is recorded because it is what the floor would fall to if the
registry ever reverted to package-level functions — not because anything here
may build below 1.27.

**What it costs, stated plainly because a consumer inherits it.** Go's module
rule is that a module cannot require a lower Go version than a module it
depends on, so **`go >= 1.27` propagates into every project that adopts any
goga package** — including the ones that never touch the registry. A developer
on an older toolchain with the default `GOTOOLCHAIN=auto` silently downloads
1.27 and builds; with `GOTOOLCHAIN=local` — hermetic CI, distribution
packaging, an air-gapped builder — it is a hard failure, and the project
installs Go 1.27 before it can adopt anything.

**What is explicitly *not* required.** No `toolchain` directive, and no
from-source golangci-lint build: stock golangci-lint v2.13.2 is built with
go1.27.0 and lints generic-method code at a Go 1.27 target cleanly, so
`go-lint` uses the upstream prebuilt action. Earlier spec text describing a
`toolchain go1.27rc2` line or an `x/tools`-bumped linter build predates GA and
is obsolete.

### 4.1 `gofmt` does not follow `GOTOOLCHAIN`

`GOTOOLCHAIN=auto` re-execs the **`go` command** into the toolchain `go.mod`
asks for. It does nothing for `gofmt`, or for any other sibling binary: those
are separate executables, and a bare `gofmt` on `PATH` stays whatever the
developer's *installed* Go shipped.

That bites here specifically. A pre-1.27 `gofmt` cannot parse
`goga/registry`'s generic methods and reports

```
registry/registry.go:204:28: method must have no type parameters
```

on a tree that is perfectly formatted. It is a **stale-tool** error, not a code
error — go1.27.0's own `gofmt` prints nothing. Run the gofmt belonging to the
toolchain the `go` command resolved:

```sh
"$(go env GOROOT)/bin/gofmt" -s -l .
```

`make fmt` / `make fmtcheck` and the `go-lint` action already do this; reach for
the line above only when invoking `gofmt` by hand.
