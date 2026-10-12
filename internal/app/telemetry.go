package app

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/fx"
)

// Telemetry owns the trace provider. Jaeger receives traces through OTLP in the
// local Compose environment; production may point the endpoint to any OTLP collector.
type Telemetry struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

func NewTelemetry(config Config) (*Telemetry, error) {
	exporter, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(config.OTLPEndpoint+"/v1/traces"))
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes("", semconv.ServiceName("wagering-api"))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return &Telemetry{provider: provider, tracer: provider.Tracer("wagering-api")}, nil
}

func RegisterTelemetry(lifecycle fx.Lifecycle, telemetry *Telemetry) {
	lifecycle.Append(fx.Hook{OnStop: telemetry.provider.Shutdown})
}

func (t *Telemetry) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(request.Context(), propagation.HeaderCarrier(request.Header))
		ctx, span := t.tracer.Start(ctx, request.Method+" "+request.URL.Path)
		defer span.End()
		span.SetAttributes(attribute.String("http.request.method", request.Method), attribute.String("url.path", request.URL.Path))
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (t *Telemetry) Start(ctx context.Context, name string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, name)
}
