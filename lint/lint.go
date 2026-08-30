// Package lint is goga's golangci-lint plugin: the mechanism by which every
// goga convention is enforced rather than merely written down.
//
// # Why this package exists
//
// Design D5 says a convention with no mechanism is a goga defect, not a
// caveat. Design D18 turns that into a delivery rule: every milestone from M1
// on ships its package *and* the linter rule that enforces that package's
// conventions, in the same change. M0 therefore ships the mechanism — this
// plugin, its [analysistest] harness, and exactly one worked analyzer — so
// that M1 has a pattern to copy rather than a mechanism to invent.
//
// The corollary matters as much as the rule: adding an analyzer here without
// the milestone whose conventions it enforces is precisely what D18 forbids.
// A rule that lands ahead of the code it governs has nothing to find, so
// nobody ever learns whether it works; a rule that lands behind it is a
// convention that was unenforced for a milestone. Both are the failure D18
// exists to prevent. Do not add an analyzer to this package because it seems
// useful — add it when its milestone lands, together with the package it
// enforces and the fixtures that prove it fires and that it stays quiet.
//
// # What ships today
//
// [NewLayoutAnalyzer] ("gogalayout"), which enforces goga's own flat layout
// (task 0.2). It is here rather than in a later milestone because it is the
// one rule M0 itself can already violate: it governs the repository M0
// creates, so M0 is its milestone. Tasks 11.1 and 11.2 group it with
// gogaparamstruct as "cross-cutting, not attributable to a module"; with the
// analyzer shipped here, 11.2 becomes a verification rather than new work.
//
// [NewSemconvAnalyzer] ("gogasemconv"), which reports an attribute key written
// as a string literal where goga/semconv already declares a constant for it
// (M1, task 1.15). It arrives with M1 exactly as D18 requires: goga/semconv is
// the package whose one rule is "never write the key at the point of use", and
// this is that rule's mechanism rather than a second statement of it.
//
// [NewServeAnalyzer] ("gogaserve"), which reports project code that constructs
// an http.Server itself or calls http.ListenAndServe (M2, task 2.14). It is
// the rule with something to enforce precisely because M2 narrowed the port to
// http.Handler: goga no longer wraps a router, so the only thing left to get
// wrong is the listener — and getting it wrong silently loses the bounded
// timeouts and the graceful drain that are the whole of what serve.New adds.
//
// M2 is also where an earlier draft had the enforcement backwards, and the
// correction generalises: it proposed depguard bans on gin-gonic/gin and
// go-chi/chi. With the port at http.Handler those routers are no longer
// wrapped, so a project's handler imports gin legitimately and the ban would
// have fired on correct code. A WRAPPER MAY NOT BAN WHAT IT DOES NOT WRAP. The
// owner's "we don't use direct dependencies" position is unchanged for every
// module that genuinely does wrap its tool — it simply cannot reach a
// dependency goga deliberately stopped abstracting.
//
// [NewConfigAnalyzer] ("gogaconfig"), which reports project code reading the
// process environment itself — os.Getenv, os.LookupEnv or os.Environ — outside
// package main (M3, task 3.12). goga/config's one guarantee is that a value
// resolves through a FIXED precedence, defaults then file then env then flags,
// and a direct read removes the top two levels of it for that one key while
// still compiling and still returning the right answer in the author's shell.
// package main is exempt because a command has to read something before there
// is any configuration to read it from; the argument for each exemption,
// including the deliberate disagreement with gogaserve over _test.go files, is
// in the analyzer's own doc.
//
// [NewMCPAnalyzer] ("gogamcp"), which reports a call on the result of SDK()
// that registers a tool, a resource or a prompt (M6, task 6.14). It is the rule
// M6 cannot do without, because goga/mcp's every guarantee — the span, the
// per-tool timeout, the panic recovery — is attached by its own AddTool, and
// the module's structure closes every route to the wrapped server except one:
// the SDK() accessor, which is a deliberate escape hatch for the large SDK
// surface the wrapper does not cover. Reaching for it is legitimate;
// REGISTERING through it is the single remaining way past the instrumentation,
// so it is the one thing the analyzer reports.
//
// M6's pairing is worth reading beside M3's. As gogaconfig pairs with the koanf
// import ban, gogamcp pairs with a depguard rule confining
// github.com/modelcontextprotocol/go-sdk to goga/mcp — and the two cover the
// two different ways the wrapper gets left behind: a server the wrapper never
// saw, and a tool registered on the one it did. The import ban also ends a
// concrete drift rather than a hypothetical one: the two adopting projects were
// pinned to two different SDK versions when M6 was written, and confining the
// import to one package makes the version a property of goga rather than
// something two repositories have to agree on.
//
// # What does not ship yet, and who owns it
//
// The remaining rules named by the spec, each owned by the milestone that
// introduces the package it governs. None of these belong in this package
// before that milestone lands.
//
//   - gogadatabase   — M4, task 4.13: sql.Open / sql.OpenDB bypassing the
//     portable type. (Replaces the gogatelemetry rule an earlier draft named.)
//   - gogacli        — M8, task 8.9: cobra.Command.Execute().
//   - gogawire       — M9, task 9.11: a goga provider constructed outside a
//     wire provider set.
//   - gogaclient     — M10, task 10.8: http.DefaultClient or a bare
//     http.Client.
//   - gogaparamstruct — M11, task 11.1: an exported constructor whose final
//     *non-variadic* parameter is a struct (or pointer to one) declared in the
//     same package with at least one exported field, and which takes no
//     variadic option parameter. The narrow predicate is the whole point: the
//     loose "final parameter is a struct" fires on New(t *testing.T) and on
//     migrate.New(db *database.DB, …), and a rule that cries wolf gets
//     disabled.
//   - gogagrpc       — M12: grpc.Dial, deprecated upstream.
//   - gogastream     — cross-cutting, docs/CONVENTIONS.md §1.3: a defer
//     cancel() in a function returning a value the caller reads
//     incrementally. Not yet assigned a task number.
//
// Two rules the spec names are deliberately *not* implemented here, because
// both are import bans and .golangci.yml already states them as depguard
// rules. An analyzer would be a second, weaker implementation of a check the
// shipped config already performs.
//
//   - gogaviper — an import of spf13/viper, where koanf is the house choice.
//     It sits with the two other house bans in the `house` depguard rule.
//   - the koanf restriction M3's task 3.12 pairs with gogaconfig — an import
//     of knadh/koanf outside goga/config. It is the `config-owns-koanf` rule,
//     written in the same shape as `telemetry-owns-the-otel-sdk`: a files
//     exclusion rather than an allow-list, so the rule reads as what it
//     forbids.
//
// The pairing is worth reading as a whole. gogaconfig and the koanf
// restriction cover the two different ways a project leaves the precedence
// behind — reading the environment underneath goga/config, and building a
// second loader beside it — and neither check can see the other's case.
//
// # Using the plugin
//
// The plugin registers itself under the name "goga" with golangci-lint's
// plugin-module-register. Build a custom golangci-lint binary with
// .custom-gcl.yml (`golangci-lint custom`), then enable it from
// .golangci.yml under linters.settings.custom.goga. [New] is the entry point
// that builds the analyzer set from the decoded configuration.
//
// [analysistest]: https://pkg.go.dev/golang.org/x/tools/go/analysis/analysistest
package lint

import (
	"fmt"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
)

// Name is the name golangci-lint knows this plugin by: the key under
// linters.settings.custom in .golangci.yml, and the linter name suffixed to
// every finding as "(goga)".
//
// An analyzer here does NOT prefix its own message with its own name.
// golangci-lint already renders a plugin finding as
// "<analyzer>: <message> (<plugin>)", so a self-prefix produces
// "gogalayout: gogalayout: …". Both halves of the attribution are already
// there — which analyzer fired, and which plugin it came from.
const Name = "goga"

func init() {
	register.Plugin(Name, newPlugin)
}

// Settings is the decoded form of the plugin's golangci-lint configuration —
// the `settings:` block under linters.settings.custom.goga.
//
// It is decoded with [register.DecodeSettings], which rejects unknown fields.
// That is not incidental: golangci-lint silently ignores unknown keys in its
// own settings blocks, so a typo produces a linter that reports zero issues
// and looks perfectly healthy. Failing the build on an unrecognised key is the
// only way a misconfiguration here is visible.
type Settings struct {
	// ModulePrefix is the import-path prefix of the module being linted. It
	// scopes every analyzer here to the module's own packages, so that a
	// dependency's layout and a dependency's attribute keys are never
	// reported; see [NewLayoutAnalyzer] and [NewSemconvAnalyzer]. Empty means
	// [DefaultModulePrefix].
	ModulePrefix string `json:"module-prefix"`
}

// New builds the analyzers this plugin contributes. It is the entry point
// golangci-lint reaches through plugin-module-register, and it is exported so
// that a test — or a downstream project embedding goga's rules in its own
// custom binary — can build the same set without going through the registry.
func New(conf any) ([]*analysis.Analyzer, error) {
	settings, err := register.DecodeSettings[Settings](conf)
	if err != nil {
		return nil, fmt.Errorf("goga/lint: decoding %q plugin settings: %w", Name, err)
	}

	// Every analyzer takes the same module prefix: it answers the same question
	// for each of them — which packages belong to the module under analysis —
	// and a second setting spelling the same module path twice is a second
	// place for an adopting project to get it half right.
	return []*analysis.Analyzer{
		NewLayoutAnalyzer(settings.ModulePrefix),
		NewSemconvAnalyzer(settings.ModulePrefix),
		NewServeAnalyzer(settings.ModulePrefix),
		NewConfigAnalyzer(settings.ModulePrefix),
		NewMCPAnalyzer(settings.ModulePrefix),
	}, nil
}

// plugin adapts [New] to the register.LinterPlugin interface. The settings are
// held undecoded until BuildAnalyzers because that is when golangci-lint wants
// the error.
type plugin struct{ conf any }

func newPlugin(conf any) (register.LinterPlugin, error) {
	return &plugin{conf: conf}, nil
}

func (p *plugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return New(p.conf)
}

// GetLoadMode reports that golangci-lint must type-check a package before
// running these analyzers. Not one of them reads pass.TypesInfo — each resolves
// the packages it cares about from the file's own import block, deliberately —
// but every one of them is SCOPED by pass.Pkg.Path(), and that is what forces
// the mode.
//
// An earlier revision of this comment claimed the opposite: that "golangci-lint
// sets pass.Pkg from the package path even in syntax mode". It does not. In
// v2.13.2, runner_loadingpackage.go returns from the syntax path *before*
// `pkg.Types = types.NewPackage(pkg.PkgPath, pkg.Name)`, so under
// register.LoadModeSyntax pass.Pkg is nil, every analyzer here takes its
// `if pass.Pkg == nil` guard on the first line, and the plugin reports zero
// issues while looking perfectly healthy.
//
// That is the trap .golangci.yml documents at the top of the file, reached by a
// different route — and it survived M0 and M1 because analysistest does not
// exercise this path (it always type-checks) and CI runs the stock binary,
// which cannot load the plugin at all. The only way to catch it is to build the
// custom binary and watch a rule fire:
//
//	golangci-lint custom && ./bin/goga-gcl run ./...
//
// Do that after any change here. Raising the mode costs effectively nothing:
// the shipped config enables staticcheck, govet and revive, every one of which
// needs type information anyway, so the package is type-checked regardless of
// what this plugin asks for.
func (p *plugin) GetLoadMode() string { return register.LoadModeTypesInfo }
