package otelsqletch

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/moznion/go-sqletch/runtime"
)

// TraceObserver decorates another observer with span attributes on
// the ambient trace span: full-cardinality shape data belongs on
// traces, never on metric labels (doc 18 §7). Compose events carry no
// context (the cache API takes none) and pass through untouched — the
// exec event repeats the full key, so the span loses nothing.
//
// Next may be nil to record span attributes only.
type TraceObserver struct {
	Next runtime.Observer
}

var _ runtime.Observer = TraceObserver{}

// ObserveCompose forwards; there is no context to attach to.
func (t TraceObserver) ObserveCompose(query string, key runtime.ShapeKey, hit bool) {
	if t.Next != nil {
		t.Next.ObserveCompose(query, key, hit)
	}
}

// ObserveExec stamps the query, the full canonical shape key, and the
// row count onto the recording span, then forwards.
func (t TraceObserver) ObserveExec(ctx context.Context, query, shapeKey string, d time.Duration, rows int64, err error) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(
			attribute.String("sqletch.query", query),
			attribute.String("sqletch.shape_key", shapeKey),
			attribute.Int64("sqletch.rows", rows),
		)
	}
	if t.Next != nil {
		t.Next.ObserveExec(ctx, query, shapeKey, d, rows, err)
	}
}

// ObserveReject records the refusal as a span event, then forwards.
func (t TraceObserver) ObserveReject(ctx context.Context, query string, err error) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.AddEvent("sqletch.reject", trace.WithAttributes(
			attribute.String("sqletch.query", query),
			attribute.String("sqletch.reject_error", rejectName(err)),
		))
	}
	if t.Next != nil {
		t.Next.ObserveReject(ctx, query, err)
	}
}
