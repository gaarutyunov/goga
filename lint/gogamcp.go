package lint

import (
	"go/ast"
	"go/token"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// mcpSDKImportPath is the upstream Model Context Protocol SDK that goga/mcp
// wraps. As everywhere else in this plugin the package-level half of the rule
// matches on the import path rather than on the identifier "mcp", which matters
// more here than anywhere: goga's own wrapper package is ALSO called mcp, so a
// file that uses both has aliased one of them and the identifier says nothing
// about which package a selector reached.
const mcpSDKImportPath = "github.com/modelcontextprotocol/go-sdk/mcp"

// mcpOwnerPackage is the one package inside the module that is ALLOWED to
// register on the SDK server directly: goga/mcp, and everything beneath it.
// Reaching the wrapped server is what that subtree does — it is the package
// that OWNS the escape hatch, and a rule firing there would be telling the
// wrapper not to wrap.
//
// Like [serveOwnerPackage], [configOwnerPackage] and [databaseOwnerPackage] it
// is matched against the path relative to the module prefix rather than as a
// path segment anywhere. The exemption is a position in the tree, not a name: a
// helper called mcputil, or a nested rpc/mcp somewhere else in the module, is
// ordinary project code and gets checked like any other.
const mcpOwnerPackage = "mcp"

// mcpEscapeHatchMethod is the accessor whose result this rule is about. It is
// matched by NAME rather than by type, because this plugin loads syntax alone
// (see plugin.GetLoadMode's doc for why that is the house choice and what it
// costs); the honest consequence is recorded in [mcpDoc].
const mcpEscapeHatchMethod = "SDK"

// mcpRegistrationMethods are the methods on the SDK's own *mcp.Server that
// publish something to a client, mapped to what the diagnostic calls the thing
// being published.
//
// The set is exactly the SDK's Add* registration surface, checked against the
// real package by TestMCPTriggersExistInSDK rather than transcribed from
// memory. Two neighbouring families are deliberately ABSENT, and the reason is
// the same for both — this rule is about routing a REGISTRATION around the
// wrapper, not about touching the SDK server at all:
//
//   - AddSendingMiddleware and AddReceivingMiddleware install middleware. That
//     ADDS behaviour around what the wrapper already does rather than escaping
//     it, and it is one of the things the escape hatch exists for.
//   - RemoveTools, RemoveResources, RemoveResourceTemplates and RemovePrompts
//     take a registration away. A tool that disappears is a visible failure —
//     the client stops seeing it — which is the opposite of the silent one this
//     rule exists to catch.
var mcpRegistrationMethods = map[string]string{
	"AddTool":             "tool",
	"AddResource":         "resource",
	"AddResourceTemplate": "resource template",
	"AddPrompt":           "prompt",
}

// mcpRegistrationFuncs are the SDK's PACKAGE-LEVEL registration functions,
// mapped to the argument position that takes the server.
//
// There is exactly one today: the generic mcp.AddTool[In, Out](s, t, h), which
// is the spelling the SDK's own documentation leads with, because it is the one
// that derives the tool's JSON schema from the handler's Go types. A rule
// covering only the method form would therefore miss the COMMON spelling rather
// than an exotic one — sdkmcp.AddTool(s.SDK(), …) is what an adopter reaching
// past the wrapper would most naturally write.
//
// It is a map of positions rather than a set of names for the same reason
// [attributeKeyArg] is: a later function whose server argument is not first can
// be added here without the call site having to special-case it.
var mcpRegistrationFuncs = map[string]int{"AddTool": 0}

const mcpDoc = `check that MCP tools are registered through goga/mcp, not on the server SDK() hands back

goga/mcp wraps the Model Context Protocol SDK's server so that ONE property
holds of every tool the process serves: the handler runs inside a span, under a
per-tool timeout, and behind a panic recovery that turns a crashing handler into
an error result rather than a dead process. All three are attached by goga/mcp's
own AddTool, and the wrapped SDK server is an unexported field reachable through
exactly one exported accessor, SDK().

That accessor is a deliberate escape hatch, not an accident. The SDK has a large
surface goga/mcp does not wrap — sending middleware, custom methods, the
notification surface — and a wrapper that hid it would force a project to choose
between the instrumentation and the protocol. So SDK() is legitimate, and this
analyzer does not report reaching for it.

What it reports is the ONE use of that hatch which is never legitimate:
REGISTERING through it. sdkmcp.AddTool(s.SDK(), …), s.SDK().AddTool(…),
.AddResource(…), .AddResourceTemplate(…) and .AddPrompt(…) all publish something
to the client on the wrapped server, underneath the wrapper, and everything the
wrapper guarantees is simply absent for that one tool. This is the SINGLE route
around the instrumentation that the module's structure leaves open — the field
is unexported and New is the only constructor, so there is no other way to hold
the SDK server — which is why it has an analyzer rather than a paragraph.

WHY THIS IS NOT A STYLE RULE. A tool registered this way works. It appears in
tools/list, it answers tools/call, and it returns the right results. What it
does not do is emit a span, stop at the timeout, or survive a panic — and none
of those absences is visible at the call site, in the type, or in any test that
only checks the tool's output. The failure surfaces as a request that hangs
forever, or as a process that dies on one bad input, long after the line that
caused it shipped. Structural enforcement cannot reach it: the SDK's server is
the SDK's type, and by the time SDK() has returned it, its Add* methods are as
callable as they are on a server nobody wrapped.

The depguard half of the same job, and why it cannot do this half. A
".golangci.yml" rule confines the github.com/modelcontextprotocol/go-sdk import
path to goga/mcp, so a project cannot build a SECOND, unwrapped server beside
goga's. That rule and this analyzer cover the two different ways the wrapper
gets left behind — a server the wrapper never saw, and a tool registered on the
one it did — and neither check can see the other's case. In particular the ban
does not subsume this rule: the types a registration needs can reach a file
through an alias, a local declaration or a helper's return value, so the bypass
is writable in a file whose only import is goga/mcp. That is why the method half
of this rule does not look at the import block at all.

TWO GAPS, stated rather than hidden. The first is a rule that fires where goga
has nothing to offer. goga/mcp wraps three of the four registrations — AddTool,
AddResource and AddPrompt each have a counterpart taking a name, a description
and a plain Go function — but there is NO counterpart for
AddResourceTemplate today, so a project that genuinely needs one has no
instrumented call to move to and this rule will still report it. That call takes
a //nolint with a reason, which is the same trade gogadatabase makes for the
non-PostgreSQL sql.Open it cannot offer a replacement for: one annotation a
reviewer can see, rather than a silent permission for every uninstrumented
registration in the tree. The right fix is a wrapper for it, not a narrower
rule.

The second is the client. goga/mcp's Client has an SDK() accessor of its own,
returning a session rather than a server, so the shape this rule matches occurs
on the client half too — and stays quiet there, because a session publishes
nothing to anybody and has no registration surface to route around. That is a
consequence of the trigger set rather than an exclusion, and it is pinned by a
fixture so it stays one.

WHAT THIS RULE DOES NOT SEE, stated rather than hidden. The match is syntactic,
because this plugin loads syntax alone. Two shapes are recognised: the call
chained onto SDK() directly, and a local variable assigned from a SDK() call in
the same function body, which is the one-line edit that would otherwise evade
the first. A server that travels further than that — stored in a struct field,
passed as a parameter, returned from a helper — is not tracked, and neither is a
lookalike SDK() on an unrelated type that also happens to declare a method
called AddTool. The first is a false negative and the second a false positive;
both need the type information the plugin deliberately does not load, and both
are rarer than the shapes this rule does catch.

Scope, and the exclusions. Only packages belonging to the module named by
-module-prefix are checked, so an adopting project points the same rule at its
own module and never sees a dependency's registrations. goga/mcp and everything
under it are exempt: that subtree owns the wrapped server, so every one of these
calls appears there by construction. testdata/ and vendor/ are skipped for the
same reason as everywhere else in this plugin.

_test.go files are NOT exempt, BY DECISION, siding with gogaserve and
gogadatabase rather than with gogaconfig — and the disagreement between those is
what makes the choice legible. gogaconfig exempts test files because the defect
it names CANNOT OCCUR in one: a test binary has no config file and no flags, so
there is no precedence for an environment read to bypass. Here the defect occurs
identically in a test, and worse than identically: a tool registered on the raw
SDK server in a test is a tool whose timeout and panic recovery the test never
exercises, so the test passes while proving nothing about the path that ships.
goga/mcp's AddTool is a drop-in for the SDK's, so there is no cost to the
alternative, and the one place a test legitimately drives the raw server —
goga/mcp's own — is already inside the owner exemption. A test elsewhere that
genuinely needs a raw registration writes a //nolint with a reason, which a
reviewer can see.`

// mcpChecker holds the analyzer's one setting; per-instance rather than
// package-level for the same reason as [layoutChecker], [semconvChecker],
// [serveChecker], [configChecker] and [databaseChecker], so that two analyzers
// configured with different prefixes cannot interfere.
type mcpChecker struct {
	// modulePrefix is the import-path prefix of the module under analysis.
	modulePrefix string
}

// NewMCPAnalyzer builds the gogamcp analyzer. An empty modulePrefix means
// [DefaultModulePrefix].
//
// It mirrors the other constructors exactly, including the constructor-argument
// and -module-prefix-flag pair bound to the same field: the argument is how the
// golangci-lint plugin configures the rule, the flag is how singlechecker, go
// vet and analysistest do, and a test asserts the two cannot come apart.
func NewMCPAnalyzer(modulePrefix string) *analysis.Analyzer {
	analyzer, _ := newMCPAnalyzer(modulePrefix)
	return analyzer
}

// newMCPAnalyzer also returns the checker the analyzer and its flag share, so a
// test can assert that configuring the rule through either path reaches the
// same field.
func newMCPAnalyzer(modulePrefix string) (*analysis.Analyzer, *mcpChecker) {
	if modulePrefix == "" {
		modulePrefix = DefaultModulePrefix
	}
	checker := &mcpChecker{modulePrefix: modulePrefix}

	analyzer := &analysis.Analyzer{
		Name: "gogamcp",
		Doc:  mcpDoc,
		URL:  "https://pkg.go.dev/github.com/gaarutyunov/goga/lint",
		Run:  checker.run,
	}
	analyzer.Flags.StringVar(&checker.modulePrefix, "module-prefix", modulePrefix,
		"import-path prefix of the module to check; packages outside it are ignored")

	return analyzer, checker
}

// run walks each function body separately.
//
// There is no import-block fast path here, unlike [serveChecker.run] and
// [databaseChecker.run]. Those rules cannot fire in a file that does not import
// the package they police, so skipping such a file is free. Half of this rule
// is a call on the result of a method named SDK, and a file needs no import at
// all to write one — the wrapper is the project's own type. The walk is the
// cheap part anyway; what the per-body scoping buys is that a name bound to a
// SDK() call in one function cannot make an unrelated name in the next function
// look like the server.
func (c *mcpChecker) run(pass *analysis.Pass) (any, error) {
	if pass.Pkg == nil || c.skipPackage(pass.Pkg.Path()) {
		return nil, nil
	}

	for _, file := range pass.Files {
		// The identifiers this file bound to the SDK's own package, for the
		// package-level AddTool form. Empty for a file that never imports it,
		// which disables that half of the rule and leaves the method half.
		sdkNames := importedPackageNames(file, mcpSDKImportPath, "mcp")

		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			c.checkBody(pass, sdkNames, function.Body)
		}
	}

	return nil, nil
}

// checkBody reports every registration in body that reaches the wrapped server.
//
// It runs in two passes over the same body because the binding a call depends
// on may be written after it in source order only in pathological code, but
// reading the bindings first costs one extra walk and removes the question.
func (c *mcpChecker) checkBody(pass *analysis.Pass, sdkNames map[string]bool, body *ast.BlockStmt) {
	bindings := sdkBindings(body)

	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		c.checkMethodCall(pass, bindings, call)
		c.checkPackageCall(pass, sdkNames, bindings, call)

		return true
	})
}

// checkMethodCall reports `<sdk server>.AddTool(…)` and its siblings.
func (c *mcpChecker) checkMethodCall(pass *analysis.Pass, bindings map[string]bool, call *ast.CallExpr) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	kind, ok := mcpRegistrationMethods[selector.Sel.Name]
	if !ok || !isSDKServer(selector.X, bindings) {
		return
	}

	reportMCPRegistration(pass, call.Fun.Pos(), kind)
}

// checkPackageCall reports `sdkmcp.AddTool(<sdk server>, …)`, the SDK's
// package-level generic form.
func (c *mcpChecker) checkPackageCall(pass *analysis.Pass, sdkNames, bindings map[string]bool, call *ast.CallExpr) {
	if len(sdkNames) == 0 {
		return
	}

	name, ok := packageSelector(unwrapTypeArgs(call.Fun), sdkNames)
	if !ok {
		return
	}
	position, ok := mcpRegistrationFuncs[name]
	if !ok || position >= len(call.Args) || !isSDKServer(call.Args[position], bindings) {
		return
	}

	reportMCPRegistration(pass, call.Fun.Pos(), mcpRegistrationMethods[name])
}

// reportMCPRegistration emits the one diagnostic this rule has. The message
// does not prefix itself with the analyzer name — see [Name] for why.
func reportMCPRegistration(pass *analysis.Pass, position token.Pos, kind string) {
	pass.Reportf(position,
		"registering a %s on the server returned by SDK() routes around goga/mcp, so this %s runs with none of the span, the timeout or the panic recovery the wrapper attaches — and nothing at the call site, in the type or in a test of the handler's output can tell the two apart; register it through goga/mcp instead",
		kind, kind)
}

// isSDKServer reports whether expr denotes the wrapped SDK server: either a
// SDK() call written in place, or an identifier this function body bound to
// one.
func isSDKServer(expr ast.Expr, bindings map[string]bool) bool {
	if ident, ok := expr.(*ast.Ident); ok {
		return bindings[ident.Name]
	}
	return isSDKCall(expr)
}

// isSDKCall reports whether expr is a call of the form `<x>.SDK()`.
//
// The receiver is deliberately unconstrained. Requiring it to be goga's own
// wrapper would mean resolving its type, which this plugin does not do; and the
// method's name plus its empty argument list is already a narrow enough shape
// that the remaining ambiguity is the lookalike case [mcpDoc] admits to.
func isSDKCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == mcpEscapeHatchMethod
}

// sdkBindings returns the identifiers body assigns from a SDK() call.
//
// This is the whole of the rule's dataflow, and its narrowness is deliberate:
// one function body, one assignment, no reassignment analysis. It exists to
// close the one-line edit — `s := srv.SDK()` on the line above — that would
// otherwise turn the rule into a check on formatting. Anything further is in
// [mcpDoc]'s list of what this rule does not see.
//
// A blank identifier is skipped because it names nothing a later call can
// reach.
func sdkBindings(body *ast.BlockStmt) map[string]bool {
	var bindings map[string]bool

	ast.Inspect(body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}

		for i, right := range assignment.Rhs {
			if i >= len(assignment.Lhs) || !isSDKCall(right) {
				continue
			}
			ident, ok := assignment.Lhs[i].(*ast.Ident)
			if !ok || ident.Name == "_" {
				continue
			}
			if bindings == nil {
				bindings = make(map[string]bool, 1)
			}
			bindings[ident.Name] = true
		}

		return true
	})

	return bindings
}

// unwrapTypeArgs strips the explicit type arguments from a generic call's
// function expression, so that `sdkmcp.AddTool[In, Out](…)` is matched by the
// same selector lookup as the inferred `sdkmcp.AddTool(…)`.
//
// Both spellings occur: type inference covers the common case, and an explicit
// instantiation is what a caller writes when the handler is a bare func value.
// Without this the rule would report one and not the other, which is precisely
// the "still compiles, still passes its fixtures, silently covers less" failure
// the trigger-set guards in this package exist against.
func unwrapTypeArgs(expr ast.Expr) ast.Expr {
	switch typed := expr.(type) {
	case *ast.IndexExpr:
		return typed.X
	case *ast.IndexListExpr:
		return typed.X
	default:
		return expr
	}
}

// skipPackage reports whether pkgPath is outside this rule's remit: another
// module entirely, one of the subtrees in [skippedSegments], or the
// [mcpOwnerPackage] subtree that owns the wrapped server.
//
// It deliberately does NOT decide a _test.go exemption, because there is none:
// see [mcpDoc] for the argument.
func (c *mcpChecker) skipPackage(pkgPath string) bool {
	if pkgPath == c.modulePrefix {
		// The module root. It is inside the module and gets checked; there are
		// simply no segments below the prefix to inspect.
		return false
	}

	rel := strings.TrimPrefix(pkgPath, c.modulePrefix+"/")
	if rel == pkgPath {
		return true
	}

	if rel == mcpOwnerPackage || strings.HasPrefix(rel, mcpOwnerPackage+"/") {
		return true
	}

	for _, segment := range strings.Split(rel, "/") {
		if skippedSegments[segment] {
			return true
		}
	}

	return false
}
