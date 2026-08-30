package lint

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/gaarutyunov/goga/semconv"
	"golang.org/x/tools/go/analysis"
)

// semconvConstant is one entry of the table gogasemconv checks against: a key
// goga/semconv already declares, and the expression a caller should write
// instead of the literal.
//
// The key is taken FROM the constant rather than retyped as a string. That is
// the difference between a table that tracks semconv and a table that merely
// resembles it: if semconv's ServiceNameKey ever moves to a different upstream
// version with a different spelling, this table moves with it, and nothing
// here has to be noticed by a human.
type semconvConstant struct {
	// key is the attribute key the constant carries.
	key attribute.Key
	// replacement is what the diagnostic tells the reader to write. It names
	// the helper where one exists, because the helper is what a caller
	// actually writes at the point of use; the bare Key constant is only
	// useful when there is no helper for it.
	replacement string
}

// semconvConstants is the whole of what gogasemconv knows, indexed by the name
// of the declaration in goga/semconv.
//
// It is a hand-maintained table, and a hand-maintained table that goes stale
// is the same failure as the unknown-key trap this plugin exists to avoid: a
// rule that looks healthy and silently stops covering what it claims to. The
// guard is [TestSemconvConstantsCoverPackage], which type-checks goga/semconv
// and fails if this map and the package's exported attribute keys disagree in
// either direction — a key added to semconv with no entry here, or an entry
// here for a key semconv no longer declares.
//
// Indexing by declaration name rather than by key string is what makes that
// test able to say which constant is missing.
var semconvConstants = map[string]semconvConstant{
	"ServiceNameKey":      {semconv.ServiceNameKey, "semconv.ServiceName(…)"},
	"ServiceVersionKey":   {semconv.ServiceVersionKey, "semconv.ServiceVersion(…)"},
	"ErrorTypeKey":        {semconv.ErrorTypeKey, "semconv.ErrorType(…)"},
	"ModuleKey":           {semconv.ModuleKey, "semconv.Module(…)"},
	"OperationKey":        {semconv.OperationKey, "semconv.Operation(…)"},
	"ConfigSourcesKey":    {semconv.ConfigSourcesKey, "semconv.ConfigSources(…)"},
	"MigrationVersionKey": {semconv.MigrationVersionKey, "semconv.MigrationVersion(…)"},
	"MigrationNameKey":    {semconv.MigrationNameKey, "semconv.MigrationName(…)"},
}

// semconvKeys is [semconvConstants] inverted into the lookup the analyzer
// performs: literal key string to replacement expression. Two constants
// carrying the same key would collide here, which cannot happen while the
// staleness test holds — it derives the table from the package, and a package
// cannot declare one key under two names without the test noticing.
var semconvKeys = func() map[string]string {
	keys := make(map[string]string, len(semconvConstants))
	for _, constant := range semconvConstants {
		keys[string(constant.key)] = constant.replacement
	}
	return keys
}()

// attributeImportPath is the package whose constructors take an attribute key.
// The analyzer matches on this import path rather than on the identifier
// "attribute", so a package that happens to expose a String(key, value)
// function of its own is never mistaken for it.
const attributeImportPath = "go.opentelemetry.io/otel/attribute"

// attributeKeyArg maps each go.opentelemetry.io/otel/attribute function whose
// argument is an attribute KEY to that argument's index. Every entry is 0
// today; the map is written this way so that a constructor with a different
// shape can be added without the call site having to special-case it.
//
// Only the first argument is a key. The remaining arguments are values, and a
// value that happens to read like a key ("service.name" stored as the value of
// some other attribute) is not a violation — which is why this is a map of
// positions and not a set of function names.
var attributeKeyArg = map[string]int{
	// The conversion: attribute.Key("goga.module").String(v).
	"Key": 0,
	// The KeyValue constructors, one per attribute type.
	"String":       0,
	"StringSlice":  0,
	"Bool":         0,
	"BoolSlice":    0,
	"Int":          0,
	"IntSlice":     0,
	"Int64":        0,
	"Int64Slice":   0,
	"Float64":      0,
	"Float64Slice": 0,
	"Stringer":     0,
}

// semconvSkippedSegments mark a subtree this rule has no business judging, for
// the same reasons as gogalayout's list, plus semconv itself: a
// semantic-convention registry is where the literals are SUPPOSED to live, and
// a rule that fires on the only package that can legitimately write them tells
// its reader to replace a constant with itself.
var semconvSkippedSegments = map[string]bool{
	"testdata": true,
	"vendor":   true,
	"semconv":  true,
}

const semconvDoc = `check that attribute keys come from goga/semconv, not from string literals

goga/semconv declares every attribute key goga's telemetry emits, and the rule
that package exists to enforce is that no module writes a key as a string
literal at the point of use (semconv's own doc comment states it). This
analyzer is that rule's mechanism. It reports a call to a
go.opentelemetry.io/otel/attribute constructor — attribute.String,
attribute.Int64, attribute.Key, and the rest — whose key argument is a string
literal for which goga/semconv already declares a constant, and names the
constant to use instead.

Why, rather than what. A literal key is invisible to review, ungreppable once
it is misspelled, and impossible to rename across every module that emits it.
Worse, a misspelling does not fail: it produces a second time series beside the
one the dashboard reads, and the failure surfaces months later as a metric that
merely looks low. The constant is none of those things, and it is also the list
a reader consults to learn what goga emits.

Scope, and what it deliberately does NOT report. Only packages belonging to the
module named by -module-prefix are checked, so a dependency's attributes are
never reported; the prefix is configurable, which is what lets a project that
has adopted goga/semconv point the same rule at its own module. A key with no
goga/semconv equivalent is NOT reported: there is no correct alternative to
suggest, and a rule that fires where the reader can do nothing about it is the
kind of rule that gets switched off wholesale, taking the rest of this plugin
with it. Neither is a value argument that happens to read like a key, nor any
package under a semconv/ path segment — that is where the literals belong.`

// semconvChecker holds the analyzer's one setting; per-instance rather than
// package-level for the same reason as [layoutChecker], so that two analyzers
// configured with different prefixes cannot interfere.
type semconvChecker struct {
	// modulePrefix is the import-path prefix of the module under analysis.
	modulePrefix string
}

// NewSemconvAnalyzer builds the gogasemconv analyzer. An empty modulePrefix
// means [DefaultModulePrefix].
//
// It mirrors [NewLayoutAnalyzer] exactly, including the constructor-argument
// and -module-prefix-flag pair bound to the same field: the argument is how the
// golangci-lint plugin configures the rule, the flag is how singlechecker, go
// vet and analysistest do, and a test asserts the two cannot come apart.
func NewSemconvAnalyzer(modulePrefix string) *analysis.Analyzer {
	analyzer, _ := newSemconvAnalyzer(modulePrefix)
	return analyzer
}

// newSemconvAnalyzer also returns the checker the analyzer and its flag share,
// so a test can assert that configuring the rule through either path reaches
// the same field.
func newSemconvAnalyzer(modulePrefix string) (*analysis.Analyzer, *semconvChecker) {
	if modulePrefix == "" {
		modulePrefix = DefaultModulePrefix
	}
	checker := &semconvChecker{modulePrefix: modulePrefix}

	analyzer := &analysis.Analyzer{
		Name: "gogasemconv",
		Doc:  semconvDoc,
		URL:  "https://pkg.go.dev/github.com/gaarutyunov/goga/lint",
		Run:  checker.run,
	}
	analyzer.Flags.StringVar(&checker.modulePrefix, "module-prefix", modulePrefix,
		"import-path prefix of the module to check; packages outside it are ignored")

	return analyzer, checker
}

func (c *semconvChecker) run(pass *analysis.Pass) (any, error) {
	if pass.Pkg == nil || c.skipPackage(pass.Pkg.Path()) {
		return nil, nil
	}

	for _, file := range pass.Files {
		names := attributePackageNames(file)
		if len(names) == 0 {
			// The file cannot name an attribute constructor, so nothing in it
			// can be a violation. Skipping here rather than inside the walk is
			// what keeps this analyzer free on the overwhelming majority of
			// files, which import no telemetry at all.
			continue
		}

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			c.checkCall(pass, names, call)
			return true
		})
	}

	return nil, nil
}

// checkCall reports call if it is an attribute constructor whose key argument
// is a literal goga/semconv already covers.
func (c *semconvChecker) checkCall(pass *analysis.Pass, names map[string]bool, call *ast.CallExpr) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	pkgIdent, ok := selector.X.(*ast.Ident)
	if !ok || !names[pkgIdent.Name] {
		return
	}

	index, ok := attributeKeyArg[selector.Sel.Name]
	if !ok || index >= len(call.Args) {
		return
	}

	key, ok := stringLiteral(call.Args[index])
	if !ok {
		return
	}

	replacement, ok := semconvKeys[key]
	if !ok {
		// A key goga/semconv does not declare. Reporting it would be telling
		// the reader to use a constant that does not exist; the fix for such a
		// key is to add it to semconv, which is a review conversation and not
		// something a linter can assert.
		return
	}

	pass.Reportf(call.Args[index].Pos(),
		"attribute key %q has a constant in goga/semconv; use %s instead of a string literal",
		key, replacement)
}

// stringLiteral returns the value of expr when it is an untagged string
// literal. A concatenation, a constant reference or a conversion is not
// reported: only a literal spelled out at the call site is the defect this
// rule is about, and anything else is either already a named constant or
// something the analyzer cannot evaluate without type information — which this
// plugin deliberately does not load (see plugin.GetLoadMode).
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// attributePackageNames returns the identifiers file binds to the OpenTelemetry
// attribute package — normally just "attribute", but an alias is legal and a
// file may import the package twice under two names.
//
// A dot-import is deliberately not handled; see [importedPackageNames], which
// does the work and states why.
func attributePackageNames(file *ast.File) map[string]bool {
	return importedPackageNames(file, attributeImportPath, "attribute")
}

// skipPackage reports whether pkgPath is outside this rule's remit: another
// module entirely, or one of the subtrees in [semconvSkippedSegments].
func (c *semconvChecker) skipPackage(pkgPath string) bool {
	if pkgPath == c.modulePrefix {
		// The module root. It is inside the module and gets checked; there are
		// simply no segments below the prefix to inspect.
		return false
	}

	rel := strings.TrimPrefix(pkgPath, c.modulePrefix+"/")
	if rel == pkgPath {
		return true
	}

	for _, segment := range strings.Split(rel, "/") {
		if semconvSkippedSegments[segment] {
			return true
		}
	}

	return false
}
