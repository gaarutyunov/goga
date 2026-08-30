package mcp_test

import (
	"context"
	"crypto/rand"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"
)

// recordedSpan is one span a spanRecorder saw ended.
type recordedSpan struct {
	name       string
	attributes []attribute.KeyValue
	status     codes.Code
	// spanContext is the span's own context, which carries the trace id it
	// inherited from its parent.
	spanContext trace.SpanContext
	// parent is the span context that was in the ctx when the span started,
	// valid only when there was one. It is what the traceparent round trip is
	// asserted on: a server span whose parent is the client's span context is
	// the proof that _meta carried the trace across the hop.
	parent trace.SpanContext
}

// spanRecorder is a [trace.TracerProvider] that records the spans started
// through it, including their parentage.
//
// It is built on the OpenTelemetry API rather than on the SDK, for the reason
// goga's own lint configuration gives: the SDK belongs to goga/telemetry, a
// second provider stack beside goga's means spans landing in whichever global
// was set last, and depguard bans the import everywhere else. It is close
// kin to serve/servetest.SpanRecorder and differs in the one way this package
// needs — it propagates the parent's trace id and records the parent, which is
// what makes the traceparent assertions possible.
type spanRecorder struct {
	embedded.TracerProvider

	mu    sync.Mutex
	ended []recordedSpan
}

// newSpanRecorder installs a recorder as the process-wide tracer provider and
// restores the previous one when the test ends.
//
// A test using it must not call t.Parallel: the provider is global, and two
// recorders would record into each other.
func newSpanRecorder(t *testing.T) *spanRecorder {
	t.Helper()

	previous := otel.GetTracerProvider()
	rec := &spanRecorder{}
	otel.SetTracerProvider(rec)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	return rec
}

// Tracer implements [trace.TracerProvider].
func (r *spanRecorder) Tracer(string, ...trace.TracerOption) trace.Tracer {
	return &recordingTracer{rec: r}
}

// ended returns the spans that have ended so far, oldest first.
func (r *spanRecorder) endedSpans() []recordedSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedSpan(nil), r.ended...)
}

// find returns the one ended span with the given name.
func (r *spanRecorder) find(name string) (recordedSpan, bool) {
	for _, s := range r.endedSpans() {
		if s.name == name {
			return s, true
		}
	}
	return recordedSpan{}, false
}

// names returns the names of the spans that have ended so far, oldest first.
func (r *spanRecorder) names() []string {
	spans := r.endedSpans()
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.name
	}
	return names
}

func (r *spanRecorder) record(s recordedSpan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, s)
}

// recordingTracer is the tracer a spanRecorder hands out.
type recordingTracer struct {
	embedded.Tracer

	rec *spanRecorder
}

// Start opens a span that inherits its parent's trace id, so that a trace can
// be followed across the MCP hop.
func (t *recordingTracer) Start(
	ctx context.Context, name string, opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	parent := trace.SpanContextFromContext(ctx)

	s := &recordingSpan{
		rec:    t.rec,
		sc:     childSpanContext(parent),
		parent: parent,
		name:   name,
		attrs:  append([]attribute.KeyValue(nil), cfg.Attributes()...),
	}
	return trace.ContextWithSpan(ctx, s), s
}

// recordingSpan embeds the API's no-op span so that it satisfies the whole of
// [trace.Span] as that interface grows, and overrides only what is read back.
type recordingSpan struct {
	noop.Span

	rec    *spanRecorder
	sc     trace.SpanContext
	parent trace.SpanContext

	mu     sync.Mutex
	name   string
	attrs  []attribute.KeyValue
	status codes.Code
	ended  bool
}

func (s *recordingSpan) SpanContext() trace.SpanContext { return s.sc }

func (s *recordingSpan) IsRecording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.ended
}

func (s *recordingSpan) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

func (s *recordingSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs = append(s.attrs, kv...)
}

func (s *recordingSpan) SetStatus(code codes.Code, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = code
}

// End records the span. Ending twice records once, as the SDK does.
func (s *recordingSpan) End(...trace.SpanEndOption) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	rs := recordedSpan{
		name:        s.name,
		attributes:  append([]attribute.KeyValue(nil), s.attrs...),
		status:      s.status,
		spanContext: s.sc,
		parent:      s.parent,
	}
	s.mu.Unlock()

	s.rec.record(rs)
}

func (s *recordingSpan) TracerProvider() trace.TracerProvider { return s.rec }

// childSpanContext mints a span context under parent, keeping its trace id
// where there is one and starting a new trace where there is not.
func childSpanContext(parent trace.SpanContext) trace.SpanContext {
	var sid trace.SpanID
	// crypto/rand.Read does not fail; it panics on a broken system source.
	_, _ = rand.Read(sid[:])

	tid := parent.TraceID()
	if !tid.IsValid() {
		_, _ = rand.Read(tid[:])
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
	})
}

// attributeValue returns the value a recorded span carried under key.
func (s recordedSpan) attributeValue(key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.attributes {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}
