package servetest

import (
	"context"
	"crypto/rand"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"
)

// RecordedSpan is one span a [SpanRecorder] saw ended.
type RecordedSpan struct {
	// Name is the span's name at the moment it ended.
	Name string

	// Attributes are the attributes it carried, in the order they were set.
	Attributes []attribute.KeyValue

	// Status is the status code it ended with.
	Status codes.Code
}

// SpanRecorder is a [go.opentelemetry.io/otel/trace.TracerProvider] that
// records the spans started through it.
//
// It is built on the OpenTelemetry *API* and not on the SDK, deliberately. The
// SDK belongs to goga/telemetry — a second provider stack beside goga's means
// two TracerProviders, two shutdown paths, and spans landing in whichever one
// the global happened to be set to last — and goga's own lint configuration
// bans importing it anywhere else. An adopting project inherits that ban, which
// is the other reason this type exists here: asserting on real spans is exactly
// the thing a project should be able to do without reaching for the SDK.
//
// The recorder is not a tracing implementation. It samples nothing, propagates
// nothing and builds no trace tree; it answers "which spans were produced, and
// how many" and that is all the assertions in this package need.
//
// It is safe for concurrent use.
type SpanRecorder struct {
	embedded.TracerProvider

	mu    sync.Mutex
	ended []RecordedSpan
}

// NewSpanRecorder returns an empty recorder.
func NewSpanRecorder() *SpanRecorder { return &SpanRecorder{} }

// Tracer implements [go.opentelemetry.io/otel/trace.TracerProvider]. Every
// tracer it hands out records into the same recorder, so instrumentation scope
// does not have to be threaded through the assertions.
func (r *SpanRecorder) Tracer(string, ...trace.TracerOption) trace.Tracer {
	return &recordingTracer{rec: r}
}

// Ended returns the spans that have ended so far, oldest first.
func (r *SpanRecorder) Ended() []RecordedSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RecordedSpan(nil), r.ended...)
}

// Names returns the names of the spans that have ended so far, oldest first.
func (r *SpanRecorder) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, len(r.ended))
	for i, s := range r.ended {
		names[i] = s.Name
	}
	return names
}

// Reset drops everything recorded so far.
//
// The assertions in this package call it immediately before the request they
// are about to count, which is what lets them assert an exact number rather
// than a difference: a server's construction and shutdown emit spans of their
// own through the same provider.
func (r *SpanRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = nil
}

func (r *SpanRecorder) record(s RecordedSpan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, s)
}

// recordingTracer is the tracer a [SpanRecorder] hands out.
type recordingTracer struct {
	embedded.Tracer

	rec *SpanRecorder
}

func (t *recordingTracer) Start(
	ctx context.Context, name string, opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	s := &recordingSpan{
		rec:   t.rec,
		sc:    newSpanContext(),
		name:  name,
		attrs: append([]attribute.KeyValue(nil), cfg.Attributes()...),
	}
	return trace.ContextWithSpan(ctx, s), s
}

// recordingSpan embeds the API's no-op span so that it satisfies the whole of
// [go.opentelemetry.io/otel/trace.Span] as that interface grows, and overrides
// only the handful of methods the assertions read back.
type recordingSpan struct {
	noop.Span

	rec *SpanRecorder
	sc  trace.SpanContext

	mu     sync.Mutex
	name   string
	attrs  []attribute.KeyValue
	status codes.Code
	ended  bool
}

// SpanContext returns a valid, sampled span context. It is synthetic:
// instrumentation that checks whether it is recording before doing work — which
// otelhttp does — must see a span that says yes.
func (s *recordingSpan) SpanContext() trace.SpanContext { return s.sc }

// IsRecording reports true until the span ends.
func (s *recordingSpan) IsRecording() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.ended
}

// SetName renames the span. otelhttp uses it to replace a span's name once the
// route is known.
func (s *recordingSpan) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

// SetAttributes appends attributes to the span.
func (s *recordingSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attrs = append(s.attrs, kv...)
}

// SetStatus records the span's status code.
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
	rs := RecordedSpan{
		Name:       s.name,
		Attributes: append([]attribute.KeyValue(nil), s.attrs...),
		Status:     s.status,
	}
	s.mu.Unlock()

	s.rec.record(rs)
}

// TracerProvider returns the recorder that started this span.
func (s *recordingSpan) TracerProvider() trace.TracerProvider { return s.rec }

// newSpanContext mints a valid sampled span context. The ids are random rather
// than sequential so that nothing downstream can mistake two spans for one.
func newSpanContext() trace.SpanContext {
	var tid trace.TraceID
	var sid trace.SpanID
	// crypto/rand.Read does not fail; it panics on a broken system source.
	_, _ = rand.Read(tid[:])
	_, _ = rand.Read(sid[:])
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
	})
}
