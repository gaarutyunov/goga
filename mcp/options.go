package mcp

import (
	"fmt"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/registry"
	"github.com/gaarutyunov/goga/telemetry"
)

// settings is unexported, so that no package outside this one can name it,
// construct it or embed it. The only way a populated settings exists is
// [goga.Apply] folding a caller's options over [defaults]. It implements
// [Settings], which is the read-only view a transport gets.
type settings struct {
	name         string
	version      string
	instructions string

	toolTimeout time.Duration

	transport string
	reg       *registry.Registry
	raw       registry.Settings

	endpoint string

	auth  Authenticator
	instr *telemetry.Instrumentation
}

// The defaults a [New] with no options runs with.
const (
	// defaultName is what a server that never called [WithName] advertises.
	// It is deliberately generic: a name is a display string, and a wrong one
	// is confusing rather than broken.
	defaultName = "goga"

	// defaultVersion matches the SDK's own placeholder shape. A server that
	// ships should set its own with [WithVersion].
	defaultVersion = "0.0.0"

	// defaultToolTimeout bounds a tool call that set no timeout of its own.
	// Thirty seconds is long enough for a database query or an HTTP round trip
	// and short enough that a wedged tool does not hold a model's turn open
	// indefinitely — and, unlike the zero value, it is a bound.
	defaultToolTimeout = 30 * time.Second

	// defaultTransport is the transport a server resolves when [WithTransport]
	// was not called. It is the one transport that needs no registry and no
	// configuration, which is what lets mcp.New() with no options run.
	defaultTransport = "stdio"
)

// defaults returns the settings a [New] with no options runs with.
//
// The instrumentation handle is taken here, at construction, and resolves
// through OpenTelemetry's globals on every use — so a server built by a
// composition root before telemetry.Setup ran starts emitting the moment it
// does. See [telemetry.For].
func defaults() settings {
	return settings{
		name:        defaultName,
		version:     defaultVersion,
		toolTimeout: defaultToolTimeout,
		transport:   defaultTransport,
		instr:       telemetry.For(moduleName),
	}
}

// serverOptions is the copy across the SDK boundary: the subset of the caller's
// settings the SDK server itself needs.
func (s *settings) serverOptions() *sdkmcp.ServerOptions {
	return &sdkmcp.ServerOptions{
		Instructions: s.instructions,
		Logger:       s.instr.Logger(),
	}
}

// ServerName implements [Settings].
func (s *settings) ServerName() string { return s.name }

// ToolTimeout implements [Settings].
func (s *settings) ToolTimeout() time.Duration { return s.toolTimeout }

// Endpoint implements [Settings].
func (s *settings) Endpoint() string { return s.endpoint }

// settings implements the accessor interface transports read. The assertion is
// here rather than left to the first use so that a missing accessor is a
// compile error in this file.
var _ Settings = (*settings)(nil)

// Option configures [New]. It is an exported alias over an unexported settings
// type, so a caller can hold and pass an mcp.Option and cannot write the struct
// it mutates.
type Option = goga.Option[settings]

// WithName sets the server name advertised to clients.
func WithName(name string) Option {
	return func(s *settings) error {
		if name == "" {
			return errEmptyName
		}
		s.name = name
		return nil
	}
}

// WithVersion sets the server version advertised to clients.
func WithVersion(v string) Option {
	return func(s *settings) error {
		if v == "" {
			return errEmptyVersion
		}
		s.version = v
		return nil
	}
}

// WithInstructions sets the instructions the server offers a connecting client,
// which is where a server explains to a model how its tools are meant to be
// used together.
//
// Empty is a legitimate value — it means "no instructions", which is the
// default — so this option does not reject one.
func WithInstructions(text string) Option {
	return func(s *settings) error {
		s.instructions = text
		return nil
	}
}

// WithToolTimeout bounds every tool call the server serves.
//
// A single tool can raise or lower its own bound with [WithToolCallTimeout] at
// [AddTool]. There is no way to make a tool unbounded: a tool that never
// returns holds a session's turn open, and the wrapper is the only place that
// can be fixed once for every project.
func WithToolTimeout(d time.Duration) Option {
	return func(s *settings) error {
		if d <= 0 {
			return fmt.Errorf("goga/mcp: tool timeout must be > 0, got %s", d)
		}
		s.toolTimeout = d
		return nil
	}
}

// WithEndpoint sets the address a listening transport binds, in the form
// [net.Listen] takes: "host:port", or ":8080" for every interface.
//
// It is the module-level endpoint, read by a transport through
// [Settings.Endpoint]. A transport that carries its own endpoint in
// configuration uses that one instead; this is the value a program supplies in
// code, and it is what makes the http and sse transports usable with no
// configuration file at all.
func WithEndpoint(addr string) Option {
	return func(s *settings) error {
		if addr == "" {
			return errEmptyEndpoint
		}
		s.endpoint = addr
		return nil
	}
}

// WithAuthenticator installs an [Authenticator] in front of every incoming
// request.
func WithAuthenticator(a Authenticator) Option {
	return func(s *settings) error {
		if a == nil {
			return errNilAuthenticator
		}
		s.auth = a
		return nil
	}
}

// WithTelemetry REPLACES the module's instrumentation handle.
//
// There is no WithoutTelemetry, and there will not be one: every part of goga
// is instrumented, and the only choice a caller has is which instrumentation
// (design D6). Passing nil is an error rather than a way to opt out.
func WithTelemetry(i *telemetry.Instrumentation) Option {
	return func(s *settings) error {
		if i == nil {
			return errNilTelemetry
		}
		s.instr = i
		return nil
	}
}

// WithTransport selects the transport by the plain adapter name it was
// registered under: "stdio", "http", "sse".
//
// Every name but the default is resolved through the registry given to
// [WithTransportRegistry], so a name with no [RegisterTransport] behind it
// fails at [Server.Run] with an error naming what IS registered.
func WithTransport(name string) Option {
	return func(s *settings) error {
		if name == "" {
			return fmt.Errorf("goga/mcp: transport name must not be empty (the default is %q)", defaultTransport)
		}
		s.transport = name
		return nil
	}
}

// WithTransportRegistry injects the registry transports were contributed to.
//
// There is no package-level default registry, deliberately: a registry is a
// value the composition root creates and attaches adapters to, so that two
// servers in one process can be wired differently and a test never has to undo
// a global (design D8).
func WithTransportRegistry(r *registry.Registry) Option {
	return func(s *settings) error {
		if r == nil {
			return errNilRegistry
		}
		s.reg = r
		return nil
	}
}

// WithTransportSettings supplies the raw configuration subtree the registry
// decodes into the selected transport's own settings type.
//
// It is the seam between a configuration file and an adapter that goga cannot
// name: the keys are the adapter's, the decoding is the registry's, and this
// module only carries the map. A program with no configuration file passes
// nothing and the adapter runs on its own defaults plus [Settings].
func WithTransportSettings(raw registry.Settings) Option {
	return func(s *settings) error {
		s.raw = raw
		return nil
	}
}
