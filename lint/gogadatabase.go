package lint

import (
	"go/ast"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// databaseSQLImportPath and pgxPoolImportPath are the two packages whose
// constructors this rule is about. As everywhere else in this plugin the match
// is on the import path rather than on the identifier, so a project package
// named sql, or an alias bound to something else, is never mistaken for one of
// them.
const (
	databaseSQLImportPath = "database/sql"
	pgxPoolImportPath     = "github.com/jackc/pgx/v5/pgxpool"
)

// databaseOwnerPackage is the one package inside the module that is ALLOWED to
// call these constructors: goga/database, and everything beneath it — pgxdb/,
// which opens the pool, and sqlcdb/, which is the type seam over it. Opening
// the handle is precisely what that subtree does, and a rule that fired there
// would be telling the constructor not to construct.
//
// Like [serveOwnerPackage] and [configOwnerPackage] it is matched against the
// path relative to the module prefix, not as a path segment anywhere. The
// exemption is "the package that owns the handle", which is a position in the
// tree; a helper named databaseutil, or a nested api/database somewhere else in
// the module, is ordinary project code and gets checked like any other.
const databaseOwnerPackage = "database"

// databaseConstructor is one package's worth of trigger: the import path whose
// constructors bypass goga, the functions in it that do, and the goga entry
// point that returns the same handle instrumented.
type databaseConstructor struct {
	// importPath is matched against the file's import block.
	importPath string
	// pkgName is the package's own name. It is what the diagnostic quotes, so
	// that a file which aliased the import is still told about "sql.Open"
	// rather than about whatever local name it chose.
	pkgName string
	// funcs are the constructors in it that produce an uninstrumented handle.
	funcs map[string]bool
	// handle is the type the call returns, named as the reader writes it.
	handle string
	// replacement is the goga constructor returning that same type, instrumented.
	replacement string
}

// databaseConstructors is the rule's trigger set: every exported way to obtain
// a database handle that goga did not instrument.
//
// sql.OpenDB is in the set beside sql.Open for the reason that keeps every
// trigger set in this package honest — a rule naming only the obvious spelling
// is evaded by a one-line edit. OpenDB is the connector-taking form, which is
// what database.Open itself uses internally after wrapping the driver, so it is
// not an exotic corner: it is the exact call whose wrapping this rule exists to
// require. pgxpool.NewWithConfig sits beside pgxpool.New for the same reason,
// and is likewise what pgxdb.Open calls once it has installed otelpgx's tracer
// on the configuration.
//
// pgxpool.ParseConfig is deliberately NOT in the set: it builds a
// configuration and opens nothing, and it is a step in the correct sequence
// rather than a bypass of it. pgx.Connect and pgx.ConnectConfig are absent for
// a different reason — they return a single *pgx.Conn, and goga has no
// instrumented constructor returning one, so a rule reporting them would name a
// defect with no fix. That gap is real and is recorded in [databaseDoc] rather
// than papered over with a trigger nobody can act on.
var databaseConstructors = []databaseConstructor{
	{
		importPath:  databaseSQLImportPath,
		pkgName:     "sql",
		funcs:       map[string]bool{"Open": true, "OpenDB": true},
		handle:      "*sql.DB",
		replacement: "database.Open",
	},
	{
		importPath:  pgxPoolImportPath,
		pkgName:     "pgxpool",
		funcs:       map[string]bool{"New": true, "NewWithConfig": true},
		handle:      "*pgxpool.Pool",
		replacement: "pgxdb.Open",
	},
}

const databaseDoc = `check that a database handle is opened through goga/database, not with sql.Open

goga/database and goga/database/pgxdb exist to make one property true of every
database handle in the process: the queries it runs are traced and its pool
statistics are recorded. database.Open wraps pgx's database/sql driver with
otelsql before opening; pgxdb.Open installs otelpgx's tracer on the pool
configuration and registers the pool statistics as metrics. This analyzer
reports the four calls that hand back a handle with neither — sql.Open,
sql.OpenDB, pgxpool.New and pgxpool.NewWithConfig — in project code.

WHY THIS IS NOT A STYLE RULE, and must not be relaxed into one. Every other
goga module guarantees its instrumentation STRUCTURALLY: the only way to obtain
the module's type is through a constructor that attaches the telemetry, so
there is no serve.Server that serve.New did not build and no Instrumentation
that telemetry.For did not hand out. M4 has no port and no portable type, by
decision — database.Open returns the standard library's *sql.DB and pgxdb.Open
returns pgx's own *pgxpool.Pool, precisely so that nothing is erased and every
helper, every goose migration and every sqlc-generated query already written
against those types keeps working unchanged. The price of that decision is that
the TYPE CARRIES NO EVIDENCE OF THE CONSTRUCTOR. A *sql.DB opened by hand and a
*sql.DB from database.Open are the same type, satisfy the same interfaces, and
differ in nothing a compiler, a reviewer or a downstream package can observe —
only in whether anything is recorded. So this analyzer is the ONLY mechanism
carrying the instrumentation invariant for this module. Weakening it does not
cost a convention; it costs the guarantee, and the loss shows up as telemetry
that is simply absent rather than as an error anyone can see.

The depguard half of the same job, and why it cannot do this half. A
".golangci.yml" rule confines github.com/jackc/pgx/v5/stdlib — pgx's
database/sql driver, the one goga/database wraps — to goga/database and its
sub-packages. That rule catches the project that registers the raw driver in
order to open a handle from it. It cannot be widened to database/sql itself:
every project imports database/sql legitimately, because *sql.DB is what
database.Open RETURNS, and a ban on the import path would fire on the correct
code. Distinguishing construction from use is exactly what needs an analyzer,
which is why this one exists.

Scope, and what it deliberately does NOT report. Only packages belonging to the
module named by -module-prefix are checked, so an adopting project points the
same rule at its own module and never sees a dependency's handles.

goga/database and everything under it are exempt — pgxdb/, which opens the
pool, and sqlcdb/, which is the type seam over it. That subtree is where the
wrapping happens, so every one of these calls appears there by construction.

A reference to a constructor without calling it is not reported, and neither is
any other function of either package: sql.Drivers, sql.Register, sql.Named and
pgxpool.ParseConfig stay quiet. Only a CALL to one of the four is the defect.

_test.go files are NOT exempt, BY DECISION, siding with gogaserve rather than
with gogaconfig — and the disagreement between those two is what makes the
choice legible. gogaconfig exempts test files because the defect it names
CANNOT OCCUR in one: a test binary has no config file and no flags, so there is
no precedence for an environment read to bypass. Here the defect occurs
identically in a test — an uninstrumented handle emits no spans and no pool
metrics in a test exactly as in production — and, unlike gogaconfig's case,
goga offers a drop-in replacement that costs nothing: database.Open takes the
same DSN and returns the same *sql.DB. A rule only "cries wolf" when it fires
on code with no alternative, and that condition does not hold here.

The second half of the argument is the one specific to a module with no
portable type. A test is where an uninstrumented handle is most easily built
and then shared: a helper that opens one for a fixture is imported by the next
test, and then by a testing package that production code reuses, and nothing in
the type ever records that the telemetry was dropped at the first call. With no
structural guarantee to fall back on, an exemption for _test.go is an exemption
for the place the bypass most easily starts.

The remaining honest gap, stated rather than hidden. database.Open is
PostgreSQL-only: it wraps pgx's driver, so a project genuinely opening MySQL or
SQLite has no goga constructor to move to, and this rule will still report its
sql.Open. That call takes a //nolint with a reason, which is the same escape
hatch gogaserve leaves for a test that really does need a listener. It is the
right trade for a module whose invariant nothing else enforces: one annotation
a reviewer can see, rather than a silent permission for every uninstrumented
handle in the tree.`

// databaseChecker holds the analyzer's one setting; per-instance rather than
// package-level for the same reason as [layoutChecker], [semconvChecker],
// [serveChecker] and [configChecker], so that two analyzers configured with
// different prefixes cannot interfere.
type databaseChecker struct {
	// modulePrefix is the import-path prefix of the module under analysis.
	modulePrefix string
}

// NewDatabaseAnalyzer builds the gogadatabase analyzer. An empty modulePrefix
// means [DefaultModulePrefix].
//
// It mirrors the other four constructors exactly, including the
// constructor-argument and -module-prefix-flag pair bound to the same field:
// the argument is how the golangci-lint plugin configures the rule, the flag is
// how singlechecker, go vet and analysistest do, and a test asserts the two
// cannot come apart.
func NewDatabaseAnalyzer(modulePrefix string) *analysis.Analyzer {
	analyzer, _ := newDatabaseAnalyzer(modulePrefix)
	return analyzer
}

// newDatabaseAnalyzer also returns the checker the analyzer and its flag share,
// so a test can assert that configuring the rule through either path reaches the
// same field.
func newDatabaseAnalyzer(modulePrefix string) (*analysis.Analyzer, *databaseChecker) {
	if modulePrefix == "" {
		modulePrefix = DefaultModulePrefix
	}
	checker := &databaseChecker{modulePrefix: modulePrefix}

	analyzer := &analysis.Analyzer{
		Name: "gogadatabase",
		Doc:  databaseDoc,
		URL:  "https://pkg.go.dev/github.com/gaarutyunov/goga/lint",
		Run:  checker.run,
	}
	analyzer.Flags.StringVar(&checker.modulePrefix, "module-prefix", modulePrefix,
		"import-path prefix of the module to check; packages outside it are ignored")

	return analyzer, checker
}

func (c *databaseChecker) run(pass *analysis.Pass) (any, error) {
	if pass.Pkg == nil || c.skipPackage(pass.Pkg.Path()) {
		return nil, nil
	}

	for _, file := range pass.Files {
		bound := boundConstructors(file)
		if len(bound) == 0 {
			// The file imports neither package, so nothing in it can name one
			// of the constructors. Skipping here rather than inside the walk
			// keeps this analyzer free on every file that opens nothing, which
			// is most of them.
			continue
		}

		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				c.checkCall(pass, bound, call)
			}
			return true
		})
	}

	return nil, nil
}

// boundConstructors maps each identifier the file binds to one of the trigger
// packages onto that package's entry in [databaseConstructors]. A file may
// import both, and may alias either.
func boundConstructors(file *ast.File) map[string]*databaseConstructor {
	var bound map[string]*databaseConstructor

	for i := range databaseConstructors {
		constructor := &databaseConstructors[i]
		for name := range importedPackageNames(file, constructor.importPath, constructor.pkgName) {
			if bound == nil {
				bound = make(map[string]*databaseConstructor, 2)
			}
			bound[name] = constructor
		}
	}

	return bound
}

// checkCall reports call if it invokes one of the constructors bound in this
// file.
func (c *databaseChecker) checkCall(pass *analysis.Pass, bound map[string]*databaseConstructor, call *ast.CallExpr) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	ident, ok := selector.X.(*ast.Ident)
	if !ok {
		return
	}
	constructor, ok := bound[ident.Name]
	if !ok || !constructor.funcs[selector.Sel.Name] {
		return
	}

	pass.Reportf(call.Fun.Pos(),
		"opening a handle with %s.%s bypasses %s, so the %s it returns records nothing — no query spans and no pool statistics — and is otherwise the same type, so nothing downstream can tell the two apart; open it with %s instead",
		constructor.pkgName, selector.Sel.Name, constructor.replacement,
		constructor.handle, constructor.replacement)
}

// skipPackage reports whether pkgPath is outside this rule's remit: another
// module entirely, one of the subtrees in [skippedSegments], or the
// [databaseOwnerPackage] subtree that owns the handle.
//
// It deliberately does NOT decide a _test.go exemption, because there is none:
// see [databaseDoc] for the argument.
func (c *databaseChecker) skipPackage(pkgPath string) bool {
	if pkgPath == c.modulePrefix {
		// The module root. It is inside the module and gets checked; there are
		// simply no segments below the prefix to inspect.
		return false
	}

	rel := strings.TrimPrefix(pkgPath, c.modulePrefix+"/")
	if rel == pkgPath {
		return true
	}

	if rel == databaseOwnerPackage || strings.HasPrefix(rel, databaseOwnerPackage+"/") {
		return true
	}

	for _, segment := range strings.Split(rel, "/") {
		if skippedSegments[segment] {
			return true
		}
	}

	return false
}
