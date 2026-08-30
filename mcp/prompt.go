package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gaarutyunov/goga"
	"github.com/gaarutyunov/goga/semconv"
)

// PromptFunc is what a project writes for a prompt: it is handed the
// arguments the client supplied, decoded into In, and returns the messages the
// prompt renders to.
//
// In is an ordinary struct. MCP carries prompt arguments as strings on the
// wire, so its fields should be string-typed; a field of another type is
// decoded from the string's JSON form, which works for a number and does not
// for a struct.
type PromptFunc[In any] func(ctx context.Context, in In) ([]*sdkmcp.PromptMessage, error)

// promptSettings is one prompt's registration settings.
type promptSettings struct {
	title     string
	arguments []*sdkmcp.PromptArgument
}

// PromptOption configures one prompt at [AddPrompt].
type PromptOption = goga.Option[promptSettings]

// WithPromptTitle sets the prompt's display title.
func WithPromptTitle(title string) PromptOption {
	return func(s *promptSettings) error {
		if title == "" {
			return fmt.Errorf("goga/mcp: prompt title must not be empty")
		}
		s.title = title
		return nil
	}
}

// WithPromptArguments declares the prompt's arguments explicitly, replacing the
// list [AddPrompt] derives from In.
//
// The derived list carries a name and whether the argument is required, which
// is what a client needs to prompt for one; use this when an argument also
// needs a description, or when In is not a struct.
func WithPromptArguments(args ...*sdkmcp.PromptArgument) PromptOption {
	return func(s *promptSettings) error {
		if len(args) == 0 {
			return fmt.Errorf("goga/mcp: prompt arguments must not be empty")
		}
		s.arguments = args
		return nil
	}
}

// AddPrompt registers fn on s as the prompt called name.
//
// A prompt render is an operation, so it is wrapped exactly as a tool call is:
// the caller's trace context is restored from _meta, a span named
// goga.mcp.prompt carries the prompt name and the duration, and a panicking
// handler is recovered rather than taking the process with it (design D6). Like
// a resource read and unlike a tool call, a failure is a protocol error — the
// specification's in-band rule is about tool results, and a prompt has none.
//
// The prompt's argument list is derived from In's exported fields unless
// [WithPromptArguments] replaced it: the JSON name of each field, required
// unless the field is a pointer or carries omitempty. Deriving it is what keeps
// the declaration and the decoding from drifting apart, which is the failure a
// hand-written list produces the first time a field is renamed.
func AddPrompt[In any](s *Server, name, desc string, fn PromptFunc[In], opts ...PromptOption) {
	ps, err := goga.Apply(promptSettings{arguments: promptArguments[In]()}, opts...)
	if err != nil {
		panic(fmt.Sprintf("goga/mcp: prompt %q: %v", name, err))
	}

	s.srv.AddPrompt(
		&sdkmcp.Prompt{
			Name:        name,
			Description: desc,
			Title:       ps.title,
			Arguments:   ps.arguments,
		},
		func(ctx context.Context, req *sdkmcp.GetPromptRequest) (_ *sdkmcp.GetPromptResult, err error) {
			ctx = extractTraceContext(ctx, req.Params.Meta)
			ctx, end := s.instr.Start(ctx, "prompt", semconv.MCPPromptName(name))

			defer func() {
				if p := recover(); p != nil {
					panicked := &PanicError{Operation: "prompt", Name: name, Value: p, Stack: debug.Stack()}
					err = panicked
					s.instr.Logger().ErrorContext(ctx, "prompt handler panicked",
						"prompt", name, "panic", fmt.Sprint(p), "stack", string(panicked.Stack))
				}
				end(err)
			}()

			in, err := decodePromptArguments[In](req.Params.Arguments)
			if err != nil {
				return nil, fmt.Errorf("goga/mcp: prompt %q: %w", name, err)
			}

			messages, err := fn(ctx, in)
			if err != nil {
				return nil, fmt.Errorf("goga/mcp: rendering prompt %q: %w", name, err)
			}

			return &sdkmcp.GetPromptResult{Description: desc, Messages: messages}, nil
		})
}

// decodePromptArguments turns the string map MCP carries into the handler's
// argument struct.
//
// It goes through JSON rather than through reflection field by field, so that a
// field's json tag selects its argument name — the same tag the derived
// argument list is built from, which is what keeps the two halves in step. A
// non-string field is decoded from the string's JSON form, so "3" reaches an
// int and anything else reports which argument failed.
func decodePromptArguments[In any](args map[string]string) (In, error) {
	var in In
	if len(args) == 0 {
		return in, nil
	}

	object := make(map[string]json.RawMessage, len(args))
	for key, value := range args {
		if raw, err := rawJSONValue(value); err == nil {
			object[key] = raw
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return in, fmt.Errorf("encoding argument %q: %w", key, err)
		}
		object[key] = encoded
	}

	encoded, err := json.Marshal(object)
	if err != nil {
		return in, fmt.Errorf("encoding arguments: %w", err)
	}
	if err := json.Unmarshal(encoded, &in); err != nil {
		return in, fmt.Errorf("decoding arguments into %s: %w", reflect.TypeFor[In]().String(), err)
	}
	return in, nil
}

// rawJSONValue reports the JSON form of an argument that is not a string in the
// handler's struct — a number, a boolean, an array.
//
// It returns an error for anything that is not one of those, including a bare
// word, so that the caller falls back to encoding the argument as the string it
// actually is. That fallback is the common case: a prompt argument is a string
// on the wire and usually a string in the struct too.
func rawJSONValue(value string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.HasPrefix(trimmed, `"`) {
		return nil, fmt.Errorf("not a non-string JSON value")
	}
	var probe any
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if _, isString := probe.(string); isString {
		return nil, fmt.Errorf("a string, which is the wire form")
	}
	return json.RawMessage(trimmed), nil
}

// promptArguments derives a prompt's argument list from In's exported fields.
//
// A field is named by its json tag where it has one, and is required unless it
// is a pointer or the tag says omitempty — the two ways Go code says "this may
// be absent". A non-struct In produces no arguments, which is the right answer
// for a prompt that takes none.
func promptArguments[In any]() []*sdkmcp.PromptArgument {
	t := reflect.TypeFor[In]()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	var args []*sdkmcp.PromptArgument
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" && options == "" {
			continue
		}
		if name == "" {
			name = field.Name
		}

		optional := field.Type.Kind() == reflect.Pointer ||
			slices.Contains(strings.Split(options, ","), "omitempty")
		args = append(args, &sdkmcp.PromptArgument{Name: name, Required: !optional})
	}
	return args
}
