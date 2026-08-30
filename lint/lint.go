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
// # What does not ship yet, and who owns it
//
// The remaining rules named by the spec, each owned by the milestone that
// introduces the package it governs. None of these belong in this package
// before that milestone lands.
//
//   - gogaconfig     — M3: os.Getenv in project code outside main.
//   - gogadatabase   — M4, task 4.13: sql.Open / sql.OpenDB bypassing the
//     portable type. (Replaces the gogatelemetry rule an earlier draft named.)
//   - gogamcp        — M6, task 6.14: a call on the result of SDK().
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
// One rule the spec names is deliberately *not* implemented here: gogaviper
// (an import of spf13/viper, where koanf is the house choice). It is an
// import ban, and .golangci.yml already states it — with the two other house
// bans — as a depguard rule. An analyzer would be a second, weaker
// implementation of a check the shipped config already performs.
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
