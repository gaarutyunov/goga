package lint

import (
	"go/ast"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// netHTTPImportPath is the package whose listener this rule is about. The
// analyzer matches on this import path rather than on the identifier "http",
// so a project package named http, or an alias, is never mistaken for it — and
// net/http/httptest, which is a different import path, cannot be.
const netHTTPImportPath = "net/http"

// serveOwnerPackage is the one package inside the module that is ALLOWED to
// construct the listener: goga/serve, and everything beneath it — driver/,
// which holds the stdlib adapter, and servetest/, which drives it.
//
// It is matched against the path relative to the module prefix rather than as
// a path segment anywhere, unlike [skippedSegments]. That is deliberate. The
// exemption is not "anything called serve"; it is "the package that owns the
// listener", which is a specific position in the tree. A helper named
// serveutil, or a nested pkg/serve somewhere else in the module, is ordinary
// project code and gets checked like any other — a segment match would exempt
// both, and exempting a package because of its name is how a rule quietly
// stops covering the code it was written for.
const serveOwnerPackage = "serve"

// serveListenFuncs are the net/http functions that run a server with the
// package's own zero-valued defaults. Both are reported for the same reason:
// neither takes a timeout and neither can be shut down, because neither hands
// the caller back the *http.Server it built.
var serveListenFuncs = map[string]bool{
	"ListenAndServe":    true,
	"ListenAndServeTLS": true,
}

// serveTypeName is the net/http type whose composite literal is the other half
// of the rule.
const serveTypeName = "Server"

const serveDoc = `check that HTTP is served through goga/serve, not through net/http directly

goga/serve exists to make two properties true of every server in the process:
the read, header and write timeouts are SET rather than left unbounded, and
cancelling the context drains in-flight requests instead of dropping them
(tasks 2.1, 2.5). Both live in serve.New. This analyzer reports the two ways
project code can bypass it — a composite literal of http.Server, and a call to
http.ListenAndServe or http.ListenAndServeTLS.

Why, rather than what. An http.Server written by hand is not obviously wrong:
it compiles, it serves, and every one of its timeout fields defaults to zero,
which net/http documents as "no timeout". A client that opens a connection and
never sends a request holds it forever, and the failure surfaces as a process
that stops accepting long after the code that caused it shipped.
http.ListenAndServe is worse, because it is also unstoppable: it constructs the
server internally and returns only the error, so there is no Shutdown to call
and a deploy drops whatever was in flight. serve.New closes both, and the port
is http.Handler (design D22), so the fix costs nothing — a *gin.Engine, a
*chi.Mux, an *http.ServeMux and oapi-codegen's generated server all satisfy it
unchanged. Keep your router; pass it to serve.New.

Scope, and what it deliberately does NOT report. Only packages belonging to the
module named by -module-prefix are checked, which is what lets an adopting
project point the same rule at its own module and never see a dependency's
listener. goga/serve and everything under it are exempt: the stdlib listener IS
an *http.Server, and a rule that fired there would be telling the adapter not
to be the thing it is. Nothing in net/http/httptest is reported, and that needs
no exclusion — httptest.NewServer returns a *httptest.Server, a different type
from a different package, so the rule never sees it. Nor is a reference to the
TYPE reported: naming *http.Server is exactly what serve.Server.As asks a
caller to do (task 2.10), so ` + "`var srv *http.Server; s.As(&srv)`" + ` is correct
code and stays quiet. Only construction is the defect.

Test files are NOT exempt, by decision. See the package's own tests for the
argument; the short form is that httptest already covers the legitimate case
without tripping this rule, so what remains in a _test.go file is a real
listener, and an exemption would hide it. A test that genuinely needs one
writes a //nolint with a reason, which a reviewer can see.`

// serveChecker holds the analyzer's one setting; per-instance rather than
// package-level for the same reason as [layoutChecker] and [semconvChecker],
// so that two analyzers configured with different prefixes cannot interfere.
type serveChecker struct {
	// modulePrefix is the import-path prefix of the module under analysis.
	modulePrefix string
}

// NewServeAnalyzer builds the gogaserve analyzer. An empty modulePrefix means
// [DefaultModulePrefix].
//
// It mirrors [NewLayoutAnalyzer] and [NewSemconvAnalyzer] exactly, including
// the constructor-argument and -module-prefix-flag pair bound to the same
// field: the argument is how the golangci-lint plugin configures the rule, the
// flag is how singlechecker, go vet and analysistest do, and a test asserts the
// two cannot come apart.
func NewServeAnalyzer(modulePrefix string) *analysis.Analyzer {
	analyzer, _ := newServeAnalyzer(modulePrefix)
	return analyzer
}

// newServeAnalyzer also returns the checker the analyzer and its flag share, so
// a test can assert that configuring the rule through either path reaches the
// same field.
func newServeAnalyzer(modulePrefix string) (*analysis.Analyzer, *serveChecker) {
	if modulePrefix == "" {
		modulePrefix = DefaultModulePrefix
	}
	checker := &serveChecker{modulePrefix: modulePrefix}

	analyzer := &analysis.Analyzer{
		Name: "gogaserve",
		Doc:  serveDoc,
		URL:  "https://pkg.go.dev/github.com/gaarutyunov/goga/lint",
		Run:  checker.run,
	}
	analyzer.Flags.StringVar(&checker.modulePrefix, "module-prefix", modulePrefix,
		"import-path prefix of the module to check; packages outside it are ignored")

	return analyzer, checker
}

func (c *serveChecker) run(pass *analysis.Pass) (any, error) {
	if pass.Pkg == nil || c.skipPackage(pass.Pkg.Path()) {
		return nil, nil
	}

	for _, file := range pass.Files {
		names := importedPackageNames(file, netHTTPImportPath, "http")
		if len(names) == 0 {
			// The file cannot name http.Server or http.ListenAndServe, so
			// nothing in it can be a violation. Skipping here rather than
			// inside the walk keeps this analyzer free on every file that
			// serves nothing, which is most of them.
			continue
		}

		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CompositeLit:
				c.checkLiteral(pass, names, typed)
			case *ast.CallExpr:
				c.checkCall(pass, names, typed)
			}
			return true
		})
	}

	return nil, nil
}

// checkLiteral reports lit if it constructs an http.Server.
//
// Only a literal whose type is written at the literal is reported. An element
// of a []http.Server with its type elided carries no type node here, and
// widening the match to cover it would mean resolving types the plugin does
// not load (see plugin.GetLoadMode) to catch a shape nobody writes.
func (c *serveChecker) checkLiteral(pass *analysis.Pass, names map[string]bool, lit *ast.CompositeLit) {
	name, ok := packageSelector(lit.Type, names)
	if !ok || name != serveTypeName {
		return
	}

	pass.Reportf(lit.Type.Pos(),
		"constructing an http.Server directly bypasses serve.New, losing the bounded timeouts and the graceful drain; pass your handler to serve.New instead")
}

// checkCall reports call if it runs one of [serveListenFuncs].
func (c *serveChecker) checkCall(pass *analysis.Pass, names map[string]bool, call *ast.CallExpr) {
	name, ok := packageSelector(call.Fun, names)
	if !ok || !serveListenFuncs[name] {
		return
	}

	pass.Reportf(call.Fun.Pos(),
		"serving HTTP through http.%s bypasses serve.New, losing the bounded timeouts and the graceful drain; pass your handler to serve.New instead",
		name)
}

// packageSelector returns the selected name when expr is `<pkg>.<Name>` for one
// of the identifiers the file bound to the import path in question, so that a
// caller can match the name against its own table.
//
// Keying on the import block rather than on the identifier is what separates
// net/http from net/http/httptest and from a project package that happens to be
// called http: all three would spell a selector the same way, and only the
// import says which one it is.
func packageSelector(expr ast.Expr, names map[string]bool) (string, bool) {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkgIdent, ok := selector.X.(*ast.Ident)
	if !ok || !names[pkgIdent.Name] {
		return "", false
	}
	return selector.Sel.Name, true
}

// skipPackage reports whether pkgPath is outside this rule's remit: another
// module entirely, one of the subtrees in [skippedSegments], or the
// [serveOwnerPackage] subtree that owns the listener.
func (c *serveChecker) skipPackage(pkgPath string) bool {
	if pkgPath == c.modulePrefix {
		// The module root. It is inside the module and gets checked; there are
		// simply no segments below the prefix to inspect.
		return false
	}

	rel := strings.TrimPrefix(pkgPath, c.modulePrefix+"/")
	if rel == pkgPath {
		return true
	}

	if rel == serveOwnerPackage || strings.HasPrefix(rel, serveOwnerPackage+"/") {
		return true
	}

	for _, segment := range strings.Split(rel, "/") {
		if skippedSegments[segment] {
			return true
		}
	}

	return false
}

// importedPackageNames returns the identifiers file binds to importPath —
// normally just pkgName, the package's own name, but an alias is legal and a
// file may import the same package twice under two names.
//
// A dot-import is deliberately not handled, for every analyzer that uses this.
// It would make every bare Server{…} or ListenAndServe(…) in the file a
// candidate, and telling those apart from the file's own declarations needs the
// type information this plugin does not load. A dot-import is also something
// revive's dot-imports rule already reports, so the case cannot arise in a
// project running the shipped .golangci.yml.
func importedPackageNames(file *ast.File, importPath, pkgName string) map[string]bool {
	var names map[string]bool

	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}

		name := pkgName
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				continue
			}
			name = spec.Name.Name
		}

		if names == nil {
			names = make(map[string]bool, 1)
		}
		names[name] = true
	}

	return names
}
