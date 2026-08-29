package lint

import (
	"strings"

	"golang.org/x/tools/go/analysis"
)

// DefaultModulePrefix is the import-path prefix gogalayout assumes when none
// is configured: goga's own module.
const DefaultModulePrefix = "github.com/gaarutyunov/goga"

// bannedSegments are the path segments goga's own layout does not use. They are
// matched as whole segments, so a package named "internalstuff" or "pkgutil" is
// untouched — only a real internal/ or pkg/ directory is a violation.
var bannedSegments = map[string]bool{"internal": true, "pkg": true}

// skippedSegments mark a subtree the analyzer has no business judging. The go
// tool never loads a package under testdata/, and a vendored package keeps its
// upstream import path rather than a vendor/ segment, so in practice neither
// appears; the check is here so that a tool which loads packages differently
// cannot turn a fixture or a vendored dependency into a goga layout violation.
var skippedSegments = map[string]bool{"testdata": true, "vendor": true}

const layoutDoc = `check that goga's own code is laid out flat

goga has no pkg/ directory and no internal/ directory (task 0.2). Every module
is a top-level package: goga/config, goga/database, goga/serve. This analyzer
reports any file whose package lives under a pkg/ or internal/ path segment.

Why, rather than what. goga is a framework: its packages exist to be imported.
An internal/ directory is a compiler-enforced statement that a package may not
be imported from outside the module, which for a framework means the package
cannot do the one job it has. A pkg/ directory is the opposite problem — it is
pure ceremony, adding a segment to every import path that distinguishes nothing,
because in a repository that is only a library there is no non-library half for
pkg/ to be contrasted with. Flat also keeps the import path the reader sees
("goga/serve") identical to the module name they were told to use, which is the
property that makes the docs and the code agree.

Scope. Only packages belonging to the module named by -module-prefix are
checked, so a dependency's own internal/ packages are never reported. The
prefix defaults to goga's module path and is configurable, which is what makes
this rule reusable by a project that has adopted the same layout.`

// layoutChecker holds the analyzer's one setting. It is per-instance rather
// than package-level state so that constructing two analyzers with different
// prefixes — which the tests do — cannot have them interfere.
type layoutChecker struct {
	// modulePrefix is the import-path prefix of the module under analysis.
	modulePrefix string
}

// NewLayoutAnalyzer builds the gogalayout analyzer. An empty modulePrefix means
// [DefaultModulePrefix].
//
// The prefix is both a constructor argument and a -module-prefix flag on the
// analyzer's FlagSet, bound to the same field: the argument is how the
// golangci-lint plugin configures it, the flag is how singlechecker, go vet
// and analysistest do. Deriving the module path from the environment instead
// was rejected — an analysis.Pass sees an import path and a set of file names,
// not a go.mod, so any derivation would be a guess that fails exactly where a
// wrong answer is most expensive (a nested module, a GOPATH-mode fixture tree).
// A declared prefix is unambiguous, and making it configurable is also what
// lets a downstream project point the same rule at its own module.
func NewLayoutAnalyzer(modulePrefix string) *analysis.Analyzer {
	analyzer, _ := newLayoutAnalyzer(modulePrefix)
	return analyzer
}

// newLayoutAnalyzer also returns the checker the analyzer and its flag share,
// so a test can assert that setting -module-prefix actually reaches the field
// the constructor argument writes. If those two ever came apart, configuring
// the analyzer through one path would silently do nothing through the other.
func newLayoutAnalyzer(modulePrefix string) (*analysis.Analyzer, *layoutChecker) {
	if modulePrefix == "" {
		modulePrefix = DefaultModulePrefix
	}
	checker := &layoutChecker{modulePrefix: modulePrefix}

	analyzer := &analysis.Analyzer{
		Name: "gogalayout",
		Doc:  layoutDoc,
		URL:  "https://pkg.go.dev/github.com/gaarutyunov/goga/lint",
		Run:  checker.run,
	}
	analyzer.Flags.StringVar(&checker.modulePrefix, "module-prefix", modulePrefix,
		"import-path prefix of the module to check; packages outside it are ignored")

	return analyzer, checker
}

func (c *layoutChecker) run(pass *analysis.Pass) (any, error) {
	if pass.Pkg == nil {
		// Nothing to judge: the package path is the whole input to this rule.
		return nil, nil
	}

	segment := c.offendingSegment(pass.Pkg.Path())
	if segment == "" {
		return nil, nil
	}

	// Report once per file, at the package clause. The offence belongs to the
	// directory, but every file in it has to move, and a per-file diagnostic is
	// the only position an editor can take the reader to.
	for _, file := range pass.Files {
		pass.Reportf(file.Name.Pos(),
			"package %q lives inside %q; goga's own code is laid out flat — no pkg/ and no internal/ directories — so move it to a top-level package under %q",
			pass.Pkg.Path(), segment+"/", c.modulePrefix)
	}

	return nil, nil
}

// offendingSegment returns the banned path segment pkgPath sits under, or "" if
// the package is not a layout violation.
//
// The import path is the input rather than the on-disk directory because the
// import path is what carries module identity: it is what distinguishes goga's
// own internal/ from a dependency's, and it is stable regardless of where the
// module happens to be checked out.
func (c *layoutChecker) offendingSegment(pkgPath string) string {
	rel := strings.TrimPrefix(pkgPath, c.modulePrefix+"/")
	if rel == pkgPath {
		// Either another module entirely, or the module root itself — which has
		// no segments below the prefix and so cannot violate the rule.
		return ""
	}

	for _, segment := range strings.Split(rel, "/") {
		if skippedSegments[segment] {
			return ""
		}
		if bannedSegments[segment] {
			return segment
		}
	}

	return ""
}
