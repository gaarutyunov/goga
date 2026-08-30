package database_test

import (
	"go/types"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// The types a caller can run a query on. Every exported function that hands one
// back is a path out of this module, and every one of them has to be
// instrumented.
const (
	sqlDBType  = "*database/sql.DB"
	pgxPoolTyp = "*github.com/jackc/pgx/v5/pgxpool.Pool"
)

// modulePath is goga's module path; the three database packages hang off it.
const modulePath = "github.com/gaarutyunov/goga"

// instrumentedConstructors is the complete list of exported functions in this
// module that may return a database handle.
//
// It is a list of two, and that is the assertion: [TestNoExportedPathReturnsAnUninstrumentedHandle]
// walks the type-checked packages and fails if any other exported function's
// results contain a *sql.DB or a *pgxpool.Pool. A later revision that adds a
// SQLDB(pool) bridge, a New(...) (DBTX, error) constructor or a
// Register(scheme, opener) table therefore fails here rather than being noticed
// in review, which is the point: the guarantee this module makes is about its
// *exported surface*, so the test has to be about the exported surface too and
// not about the two constructors somebody remembered to check.
var instrumentedConstructors = map[string]string{
	modulePath + "/database":       "Open",
	modulePath + "/database/pgxdb": "Open",
}

// exportedSurface pins every exported name of the three packages.
//
// A pinned surface is how "there is no adapter table" stays true. Register,
// Schemes and UnknownSchemeError are not banned by a rule somewhere; they simply
// cannot be added without editing this list, and editing this list is a diff a
// reviewer sees.
var exportedSurface = map[string][]string{
	modulePath + "/database": {
		"DSN", "Open", "Option", "Set", "Tx", "TxOption",
		"WithConnMaxLifetime", "WithMaxIdleConns", "WithMaxOpenConns",
		"WithSQLCommenter", "WithTelemetry",
		"WithTxIsolation", "WithTxReadOnly", "WithTxTimeout",
	},
	modulePath + "/database/pgxdb": {
		"Open", "Option", "Set", "Tx", "TxOption",
		"WithMaxConns", "WithMinConns", "WithTelemetry",
		"WithTxIsolation", "WithTxReadOnly", "WithTxTimeout",
	},
	modulePath + "/database/sqlcdb": {
		"DBTX",
	},
}

// TestNoExportedPathReturnsAnUninstrumentedHandle is the structural half of the
// module's one guarantee.
//
// The runtime half — that the handle [github.com/gaarutyunov/goga/database.Open]
// returns really is wrapped — is asserted in database_test.go and
// pgxdb_test.go. This half asserts the other side of the same claim: that those
// two constructors are the *only* exported way to obtain a handle at all, so
// that proving them instrumented proves the package instrumented.
func TestNoExportedPathReturnsAnUninstrumentedHandle(t *testing.T) {
	for _, pkg := range loadDatabasePackages(t) {
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			if !obj.Exported() {
				continue
			}
			fn, ok := obj.(*types.Func)
			if !ok {
				continue
			}
			if !returnsAHandle(fn.Signature()) {
				continue
			}
			assert.Equal(t, instrumentedConstructors[pkg.PkgPath], name,
				"%s.%s returns a database handle; the only exported function in this package "+
					"allowed to do that is the instrumented constructor", pkg.PkgPath, name)
		}
	}
}

// TestExportedSurfaceIsPinned fails when a name is added to or removed from any
// of the three packages.
//
// It is what keeps the deleted design deleted. Register, Schemes,
// UnknownSchemeError, SQLDB and ErrNotPgx were all specified in an earlier
// revision and all removed for reasons recorded in the package documentation; a
// pinned list is the cheapest thing that notices one of them coming back.
func TestExportedSurfaceIsPinned(t *testing.T) {
	for _, pkg := range loadDatabasePackages(t) {
		want, ok := exportedSurface[pkg.PkgPath]
		require.True(t, ok, "unexpected package %s", pkg.PkgPath)

		var got []string
		for _, name := range pkg.Types.Scope().Names() {
			if pkg.Types.Scope().Lookup(name).Exported() {
				got = append(got, name)
			}
		}
		slices.Sort(got)
		slices.Sort(want)
		assert.Equal(t, want, got, "exported surface of %s", pkg.PkgPath)
	}
}

// TestThereIsNoSchemeTable states the deleted design as an assertion rather than
// only as prose.
//
// The names are checked in every one of the three packages, exported or not: a
// scheme table hidden behind an unexported map with an exported Open in front of
// it would satisfy the two tests above and still be the design that was
// reversed.
func TestThereIsNoSchemeTable(t *testing.T) {
	banned := []string{"Register", "Schemes", "UnknownSchemeError", "SQLDB", "ErrNotPgx", "Unwrap"}

	for _, pkg := range loadDatabasePackages(t) {
		for _, name := range pkg.Types.Scope().Names() {
			assert.NotContains(t, banned, name,
				"%s declares %s; there is no port to select an implementation of, so there is "+
					"no adapter table, no scheme and no bridge — see the package documentation",
				pkg.PkgPath, name)
		}
	}
}

// returnsAHandle reports whether any result of sig is a type a caller can run a
// query on.
func returnsAHandle(sig *types.Signature) bool {
	results := sig.Results()
	for i := range results.Len() {
		switch results.At(i).Type().String() {
		case sqlDBType, pgxPoolTyp:
			return true
		}
	}
	return false
}

// loadDatabasePackages type-checks the three packages of this module.
//
// Full type checking, unlike the syntax-only walk in goga/telemetry's
// instrumentation test: the question here is what an exported function's results
// actually are, and a signature's types cannot be resolved from syntax.
func loadDatabasePackages(t *testing.T) []*packages.Package {
	t.Helper()

	// packages.LoadTypes and not LoadSyntax: the questions here are about
	// exported names and signature types, both of which are in the compiler's
	// export data, so the loader can read them instead of type-checking pgx,
	// OpenTelemetry and the rest of the dependency tree from source. Asking for
	// syntax as well makes the same three assertions take eight seconds on every
	// run, and a guard that slow is a guard somebody eventually skips.
	cfg := &packages.Config{Mode: packages.LoadTypes, Tests: false}
	pkgs, err := packages.Load(cfg, modulePath+"/database/...")
	require.NoError(t, err)
	require.Len(t, pkgs, len(exportedSurface), "the package walk found every database package")

	for _, p := range pkgs {
		require.Empty(t, p.Errors, "loading %s", p.PkgPath)
		require.NotNil(t, p.Types, "type information for %s", p.PkgPath)
		require.True(t, strings.HasPrefix(p.PkgPath, modulePath+"/database"), "unexpected package %s", p.PkgPath)
	}
	return pkgs
}
