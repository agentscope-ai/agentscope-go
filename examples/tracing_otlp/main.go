// Package main demonstrates how to wire OpenTelemetry OTLP export with agentscope-go.
//
// This example shows the recommended pattern for production tracing:
//
//  1. Set up an OTLP exporter (HTTP or gRPC) pointing at your collector
//  2. Create a TracerProvider with the exporter
//  3. Set it as the global provider
//  4. Use middleware.NewTracingMiddleware(nil) — it picks up the global provider
//
// Prerequisites:
//   - A running OTLP collector (e.g., Jaeger, Grafana Tempo, or otel-collector)
//   - go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
//   - go get go.opentelemetry.io/otel/sdk/trace
//
// Run:
//
//	export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
//	go run .
//
// This is intentionally NOT a built-in library feature. OTLP export pulls in
// the OTel SDK + exporter (~50 transitive dependencies). The agentscope-go
// library only depends on the OTel API (zero-cost when no provider is set).
// Users wire their own exporter in 10 lines as shown below.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	// In a real application, uncomment and use the following:
	//
	// import (
	//     "go.opentelemetry.io/otel"
	//     "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	//     sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// )
	//
	// ctx := context.Background()
	//
	// // 1. Create OTLP HTTP exporter (reads OTEL_EXPORTER_OTLP_ENDPOINT env)
	// exporter, err := otlptracehttp.New(ctx)
	// if err != nil {
	//     log.Fatal(err)
	// }
	//
	// // 2. Create TracerProvider with the exporter
	// tp := sdktrace.NewTracerProvider(
	//     sdktrace.WithBatcher(exporter),
	//     sdktrace.WithResource(resource.NewWithAttributes(
	//         semconv.SchemaURL,
	//         semconv.ServiceNameKey.String("my-agent-service"),
	//     )),
	// )
	// defer tp.Shutdown(ctx)
	//
	// // 3. Set as global provider
	// otel.SetTracerProvider(tp)
	//
	// // 4. Use TracingMiddleware — it picks up the global provider automatically
	// a := agent.NewUnifiedAgent("bot", "You are helpful.", chatModel,
	//     agent.WithMiddlewares(middleware.NewTracingMiddleware(nil)),
	// )
	//
	// // All agent Reply/ReplyStream calls now emit spans to your OTLP collector.
	// reply, _ := a.Reply(ctx, "Hello!")

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4318"
	}

	fmt.Println("OTLP Tracing Setup Example")
	fmt.Println("==========================")
	fmt.Println()
	fmt.Println("This example demonstrates the wiring pattern for OTLP export.")
	fmt.Println("The actual OTel SDK imports are commented out to avoid pulling")
	fmt.Println("heavy dependencies into the agentscope-go module.")
	fmt.Println()
	fmt.Printf("Target endpoint: %s\n", endpoint)
	fmt.Println()
	fmt.Println("To use in your project:")
	fmt.Println("  1. go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp")
	fmt.Println("  2. go get go.opentelemetry.io/otel/sdk/trace")
	fmt.Println("  3. Copy the setup pattern from this file into your main()")
	fmt.Println("  4. Use middleware.NewTracingMiddleware(nil) on your agent")

	_ = context.Background() // suppress unused import
}
