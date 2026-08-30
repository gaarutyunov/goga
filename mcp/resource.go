package mcp

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/semconv"
)

// ResourceFunc is what a project writes for a resource: it is handed the URI
// the client asked for and returns the bytes and the MIME type.
//
// Returning the MIME type from the function rather than fixing it at
// registration is what lets one handler serve a resource whose type depends on
// what it found. A handler that always returns the same type can set it once
// with [WithResourceMIMEType] and return "" here.
type ResourceFunc func(ctx context.Context, uri string) (data []byte, mimeType string, err error)

// resourceSettings is one resource's registration settings.
type resourceSettings struct {
	title       string
	description string
	mimeType    string
}

// ResourceOption configures one resource at [AddResource].
type ResourceOption = goga.Option[resourceSettings]

// WithResourceTitle sets the resource's display title.
func WithResourceTitle(title string) ResourceOption {
	return func(s *resourceSettings) error {
		if title == "" {
			return fmt.Errorf("goga/mcp: resource title must not be empty")
		}
		s.title = title
		return nil
	}
}

// WithResourceDescription sets the description a client shows, and that a model
// reads to decide whether the resource is worth fetching.
func WithResourceDescription(text string) ResourceOption {
	return func(s *resourceSettings) error {
		if text == "" {
			return fmt.Errorf("goga/mcp: resource description must not be empty")
		}
		s.description = text
		return nil
	}
}

// WithResourceMIMEType declares the MIME type the resource always has, which a
// client can then read from the listing without fetching it.
//
// A [ResourceFunc] that returns a non-empty type overrides it for that read.
func WithResourceMIMEType(mimeType string) ResourceOption {
	return func(s *resourceSettings) error {
		if mimeType == "" {
			return fmt.Errorf("goga/mcp: resource MIME type must not be empty")
		}
		s.mimeType = mimeType
		return nil
	}
}

// AddResource registers fn on s as the resource at uri.
//
// A resource read is an operation, so it is wrapped exactly as a tool call is:
// the caller's trace context is restored from _meta, a span named
// goga.mcp.resource carries the URI and the duration, and a panicking handler
// is recovered rather than taking the process with it (design D6).
//
// It differs from [AddTool] in one place, and the difference is the
// specification's. A tool failure is in-band because the model is meant to read
// it and correct itself; a resource read that fails has no such reader, and MCP
// reports it as a protocol error. So this wrapper returns the error rather than
// packing it into a result.
func AddResource(s *Server, uri, name string, fn ResourceFunc, opts ...ResourceOption) {
	rs, err := goga.Apply(resourceSettings{}, opts...)
	if err != nil {
		panic(fmt.Sprintf("goga/mcp: resource %q: %v", uri, err))
	}

	s.srv.AddResource(
		&sdkmcp.Resource{
			URI:         uri,
			Name:        name,
			Title:       rs.title,
			Description: rs.description,
			MIMEType:    rs.mimeType,
		},
		func(ctx context.Context, req *sdkmcp.ReadResourceRequest) (_ *sdkmcp.ReadResourceResult, err error) {
			ctx = extractTraceContext(ctx, req.Params.Meta)
			ctx, end := s.instr.Start(ctx, "resource", semconv.MCPResourceURI(uri))

			defer func() {
				if p := recover(); p != nil {
					panicked := &PanicError{Operation: "resource", Name: uri, Value: p, Stack: debug.Stack()}
					err = panicked
					s.instr.Logger().ErrorContext(ctx, "resource handler panicked",
						"resource", uri, "panic", fmt.Sprint(p), "stack", string(panicked.Stack))
				}
				end(err)
			}()

			data, mimeType, err := fn(ctx, req.Params.URI)
			if err != nil {
				return nil, fmt.Errorf("goga/mcp: reading resource %q: %w", uri, err)
			}
			if mimeType == "" {
				mimeType = rs.mimeType
			}

			contents := &sdkmcp.ResourceContents{URI: req.Params.URI, MIMEType: mimeType}
			if isTextual(mimeType) {
				contents.Text = string(data)
			} else {
				contents.Blob = data
			}
			return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{contents}}, nil
		})
}

// isTextual reports whether a MIME type names content a client should receive
// as text rather than as a base64 blob.
//
// The distinction is the protocol's: ResourceContents carries either Text or
// Blob, and a client shows one and decodes the other. Sending JSON as a blob is
// not wrong on the wire and is unreadable everywhere else, so the rule is
// stated once here rather than left to every handler.
//
// The rule is deliberately narrow — the text/* tree, the three structured
// formats that are text by definition, and the +json / +xml / +yaml structured
// suffixes the IANA registry defines. Anything else, including an empty MIME
// type, is a blob, because a blob of text survives being read as text and text
// that was really a PNG does not.
func isTextual(mimeType string) bool {
	base, _, _ := strings.Cut(mimeType, ";")
	base = strings.TrimSpace(strings.ToLower(base))

	if strings.HasPrefix(base, "text/") {
		return true
	}
	switch base {
	case "application/json", "application/xml", "application/yaml":
		return true
	}
	for _, suffix := range []string{"+json", "+xml", "+yaml"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}
