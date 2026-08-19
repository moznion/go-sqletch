# otelsqletch

OpenTelemetry adapter for [sqletch](../../README.md) runtime metrics
and traces: composed-SQL cache performance, shape usage vs. verified
reachability, call latency/rows, and pre-SQL rejections.

This is a **separate Go module** so the OpenTelemetry dependency graph
never touches the core `runtime` package or consumers who skip
metrics. The core exposes only the neutral `runtime.Observer`
interface and poll-able cache snapshots; this package is one adapter
over them. Prometheus/OpenMetrics exposition is the standard OTel
Prometheus exporter's job, attached to the `MeterProvider` you pass
in; OTLP push pipelines work unchanged.

```console
go get github.com/moznion/go-sqletch/contrib/otel
```

## Quick start

```go
exporter, _ := otelprom.New()
mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))

metrics, _ := otelsqletch.New(mp)
q := gen.New(pool)
binding, _ := metrics.Bind("main", q, gen.ShapeSpace)
defer binding.Close()

http.Handle("/metrics", promhttp.Handler())
```

One `Bind` per `gen.New(...)` instance, each with its own `instance`
label. To also stamp the full shape key onto trace spans, decorate:

```go
q.SetObserver(otelsqletch.TraceObserver{Next: binding})
```

## Cardinality

The shape key never becomes a per-event metric label — its tree and
arity segments are caller-controlled and unbounded. Per-event series
are labeled by query name and small closed sets only;
full-cardinality shape data travels on trace spans (`TraceObserver`)
or the bounded `sqletch.shapes.top` snapshot. `WithTopShapes` and
`WithUsedShapeBound` tune the bounds.

## Documentation

- **[Runtime metrics manual](../../docs/manual/13-runtime-metrics.md)**
  — the full guide: instrument reference, how to read the numbers,
  prepared-statement accounting, and using the neutral surface
  without OpenTelemetry.
- [Design doc 18](../../docs/design/18-runtime-metrics.md) — the
  design and its settled decisions.
- [Package reference](https://pkg.go.dev/github.com/moznion/go-sqletch/contrib/otel)
  — API docs.
