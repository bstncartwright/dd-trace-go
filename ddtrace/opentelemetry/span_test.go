// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package opentelemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/httpmem"

	"github.com/stretchr/testify/assert"
	"github.com/tinylib/msgp/msgp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func mockTracerProvider(t *testing.T, opts ...tracer.StartOption) (tp *TracerProvider, payloads chan string, cleanup func()) {
	payloads = make(chan string)
	s, c := httpmem.ServerAndClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0.4/traces":
			if h := r.Header.Get("X-Datadog-Trace-Count"); h == "0" {
				return
			}
			buf, err := io.ReadAll(r.Body)
			if err != nil || len(buf) == 0 {
				t.Fatalf("Test agent: Error receiving traces")
			}
			var js bytes.Buffer
			msgp.UnmarshalAsJSON(&js, buf)
			payloads <- js.String()
		}
		w.WriteHeader(200)
	}))
	opts = append(opts, tracer.WithHTTPClient(c))
	tp = NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	return tp, payloads, func() {
		s.Close()
		tp.Shutdown()
	}
}

func waitForPayload(ctx context.Context, payloads chan string) (string, error) {
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("Timed out waiting for traces")
	case p := <-payloads:
		return p, nil
	}
}

func TestSpanResourceNameDefault(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	_, sp := tr.Start(ctx, "OperationName")
	sp.End()

	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(p, `"name":"internal"`)
	assert.Contains(p, `"resource":"OperationName"`)
}

func TestSpanSetName(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	_, sp := tr.Start(ctx, "OldName")
	sp.SetName("NewName")
	sp.End()

	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(p, strings.ToLower("NewName"))
}

func TestSpanEnd(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	name, ignoredName := "trueName", "invalidName"
	code, ignoredCode := codes.Error, codes.Ok
	msg, ignoredMsg := "error_desc", "ok_desc"
	attributes := map[string]string{"trueKey": "trueVal"}
	ignoredAttributes := map[string]string{"trueKey": "fakeVal", "invalidKey": "invalidVal"}

	_, sp := tr.Start(context.Background(), name)
	sp.SetStatus(code, msg)
	for k, v := range attributes {
		sp.SetAttributes(attribute.String(k, v))
	}
	assert.True(sp.IsRecording())

	sp.End()
	assert.False(sp.IsRecording())

	// following operations should not be able to modify the Span since the span has finished
	sp.SetName(ignoredName)
	sp.SetStatus(ignoredCode, ignoredMsg)
	for k, v := range ignoredAttributes {
		sp.SetAttributes(attribute.String(k, v))
		sp.SetAttributes(attribute.String(k, v))
	}

	tracer.Flush()
	payload, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}

	assert.Contains(payload, name)
	assert.NotContains(payload, ignoredName)
	assert.Contains(payload, msg)
	assert.NotContains(payload, ignoredMsg)
	assert.Contains(payload, `"error":1`) // this should be an error span

	for k, v := range attributes {
		assert.Contains(payload, fmt.Sprintf("\"%s\":\"%s\"", k, v))
	}
	for k, v := range ignoredAttributes {
		assert.NotContains(payload, fmt.Sprintf("\"%s\":\"%s\"", k, v))
	}
}

// This test verifies that setting the status of a span
// behaves accordingly to the Otel API spec
// (https://opentelemetry.io/docs/reference/specification/trace/api/#set-status)
// by checking the following:
//  1. attempts to set the value of `Unset` are ignored
//  2. description must only be used with `Error` value
//  3. setting the status to `Ok` is final and will override any
//     any prior or future status values
func TestSpanSetStatus(t *testing.T) {
	assert := assert.New(t)
	testData := []struct {
		code        codes.Code
		msg         string
		ignoredCode codes.Code
		ignoredMsg  string
	}{
		{
			code:        codes.Ok,
			msg:         "ok_description",
			ignoredCode: codes.Error,
			ignoredMsg:  "error_description",
		},
		{
			code:        codes.Error,
			msg:         "error_description",
			ignoredCode: codes.Unset,
			ignoredMsg:  "unset_description",
		},
	}
	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	for _, test := range testData {
		t.Run(fmt.Sprintf("Setting Code: %d", test.code), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var sp oteltrace.Span
			testStatus := func() {
				sp.End()
				tracer.Flush()
				payload, err := waitForPayload(ctx, payloads)
				if err != nil {
					t.Fatalf(err.Error())
				}
				// An error description is set IFF the span has an error
				// status code value. Messages related to any other status code
				// are ignored.
				if test.code == codes.Error {
					assert.Contains(payload, test.msg)
				} else {
					assert.NotContains(payload, test.msg)
				}
				assert.NotContains(payload, test.ignoredCode)
			}
			_, sp = tr.Start(context.Background(), "test")
			sp.SetStatus(test.code, test.msg)
			sp.SetStatus(test.ignoredCode, test.ignoredMsg)
			testStatus()

			_, sp = tr.Start(context.Background(), "test")
			sp.SetStatus(test.code, test.msg)
			sp.SetStatus(test.ignoredCode, test.ignoredMsg)
			testStatus()
		})
	}
}

func TestSpanContextWithStartOptions(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	startTime := time.Now()
	duration := time.Second * 5
	spanID := uint64(1234567890)
	ctx, sp := tr.Start(
		ContextWithStartOptions(context.Background(),
			tracer.ResourceName("persisted_ctx_rsc"),
			tracer.ServiceName("persisted_srv"),
			tracer.StartTime(startTime),
			tracer.WithSpanID(spanID),
		), "op_name",
		oteltrace.WithAttributes(
			attribute.String(ext.ResourceName, ""),
			attribute.String(ext.ServiceName, "discarded")),
		oteltrace.WithSpanKind(oteltrace.SpanKindProducer),
	)

	_, child := tr.Start(ctx, "child")
	ddChild := child.(*span)
	// this verifies that options passed to the parent, such as tracer.WithSpanID(spanID)
	// weren't passed down to the child
	assert.NotEqual(spanID, ddChild.DD.Context().SpanID())
	child.End()

	EndOptions(sp, tracer.FinishTime(startTime.Add(duration)))
	sp.End()

	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	if strings.Count(p, "span_id") != 2 {
		t.Fatalf("payload does not contain two spans\n%s", p)
	}
	assert.Contains(p, `"service":"persisted_srv"`)
	assert.Contains(p, `"resource":"persisted_ctx_rsc"`)
	assert.Contains(p, `"span.kind":"producer"`)
	assert.Contains(p, fmt.Sprint(spanID))
	assert.Contains(p, fmt.Sprint(startTime.UnixNano()))
	assert.Contains(p, fmt.Sprint(duration.Nanoseconds()))
	assert.NotContains(p, "discarded")
	assert.Equal(1, strings.Count(p, `"span_id":1234567890`))
}

func TestSpanContextWithStartOptionsPriorityOrder(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	startTime := time.Now()
	_, sp := tr.Start(
		ContextWithStartOptions(context.Background(),
			tracer.ResourceName("persisted_ctx_rsc"),
			tracer.ServiceName("persisted_srv"),
		), "op_name",
		oteltrace.WithTimestamp(startTime.Add(time.Second)),
		oteltrace.WithAttributes(attribute.String(ext.ServiceName, "discarded")),
		oteltrace.WithSpanKind(oteltrace.SpanKindProducer))
	sp.End()

	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(p, "persisted_ctx_rsc")
	assert.Contains(p, "persisted_srv")
	assert.Contains(p, `"span.kind":"producer"`)
	assert.NotContains(p, "discarded")
}

func TestSpanEndOptionsPriorityOrder(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	startTime := time.Now()
	_, sp := tr.Start(
		ContextWithStartOptions(context.Background(),
			tracer.ResourceName("ctx_rsc"),
			tracer.ServiceName("ctx_srv"),
			tracer.StartTime(startTime),
			tracer.WithSpanID(1234567890),
		), "op_name")

	EndOptions(sp, tracer.FinishTime(startTime.Add(time.Second)))
	// Next Calls to EndOptions do not keep previous options
	EndOptions(sp, tracer.FinishTime(startTime.Add(time.Second*5)))
	// EndOptions timestamp should prevail
	sp.End(oteltrace.WithTimestamp(startTime.Add(time.Second * 3)))
	// making sure end options don't have effect after the span has returned
	EndOptions(sp, tracer.FinishTime(startTime.Add(time.Second*2)))
	sp.End()

	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(p, `"duration":5000000000,`)
	assert.NotContains(p, `"duration":2000000000,`)
	assert.NotContains(p, `"duration":1000000000,`)
	assert.NotContains(p, `"duration":3000000000,`)
}

func TestSpanEndOptions(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	startTime := time.Now()
	_, sp := tr.Start(
		ContextWithStartOptions(context.Background(),
			tracer.ResourceName("ctx_rsc"),
			tracer.ServiceName("ctx_srv"),
			tracer.StartTime(startTime),
			tracer.WithSpanID(1234567890),
		), "op_name")

	EndOptions(sp, tracer.FinishTime(startTime.Add(time.Second*5)),
		tracer.WithError(errors.New("persisted_option")))
	sp.End()
	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(p, "ctx_srv")
	assert.Contains(p, "ctx_rsc")
	assert.Contains(p, "1234567890")
	assert.Contains(p, fmt.Sprint(startTime.UnixNano()))
	assert.Contains(p, `"duration":5000000000,`)
	assert.Contains(p, `persisted_option`)
	assert.Contains(p, `"error":1`)
}

func TestSpanSetAttributes(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	attributes := [][]string{{"k1", "v1_old"},
		{"k2", "v2"},
		{"k1", "v1_new"},
		// maps to 'name'
		{"operation.name", "ops"},
		// maps to 'service'
		{"service.name", "srv"},
		// maps to 'resource'
		{"resource.name", "rsr"},
		// maps to 'type'
		{"span.type", "db"},
	}

	_, sp := tr.Start(context.Background(), "test")
	for _, tag := range attributes {
		sp.SetAttributes(attribute.String(tag[0], tag[1]))
	}
	// maps to '_dd1.sr.eausr'
	sp.SetAttributes(attribute.Int("analytics.event", 1))

	sp.End()
	tracer.Flush()
	payload, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(payload, `"k1":"v1_new"`)
	assert.Contains(payload, `"k2":"v2"`)
	assert.NotContains(payload, "v1_old")

	// reserved attributes
	assert.Contains(payload, `"name":"ops"`)
	assert.Contains(payload, `"service":"srv"`)
	assert.Contains(payload, `"resource":"rsr"`)
	assert.Contains(payload, `"type":"db"`)
	assert.Contains(payload, `"_dd1.sr.eausr":1`)
}

func TestSpanSetAttributesWithRemapping(t *testing.T) {
	assert := assert.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	_, sp := tr.Start(ctx, "custom")
	sp.SetAttributes(attribute.String("graphql.operation.type", "subscription"))

	sp.End()

	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(p, "graphql.server.request")
}

func TestTracerStartOptions(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payloads, cleanup := mockTracerProvider(t, tracer.WithEnv("test_env"), tracer.WithService("test_serv"))
	tr := otel.Tracer("")
	defer cleanup()

	_, sp := tr.Start(context.Background(), "test")
	sp.End()
	tracer.Flush()
	payload, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(payload, "\"service\":\"test_serv\"")
	assert.Contains(payload, "\"env\":\"test_env\"")
}

func TestOperationNameRemapping(t *testing.T) {
	assert := assert.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payloads, cleanup := mockTracerProvider(t)
	tr := otel.Tracer("")
	defer cleanup()

	_, sp := tr.Start(ctx, "operation", oteltrace.WithAttributes(attribute.String("graphql.operation.type", "subscription")))
	sp.End()

	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(p, "graphql.server.request")
}
func TestRemapName(t *testing.T) {
	testCases := []struct {
		spanKind oteltrace.SpanKind
		in       []attribute.KeyValue
		out      string
	}{
		{
			in:  []attribute.KeyValue{attribute.String("operation.name", "Ops")},
			out: "ops",
		},
		{
			in:  []attribute.KeyValue{},
			out: "internal",
		},
		{
			in:       []attribute.KeyValue{attribute.String("http.request.method", "POST")},
			spanKind: oteltrace.SpanKindClient,
			out:      "http.client.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("http.request.method", "POST")},
			spanKind: oteltrace.SpanKindServer,
			out:      "http.server.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("db.system", "Redis")},
			spanKind: oteltrace.SpanKindClient,
			out:      "redis.query",
		},
		{
			in:       []attribute.KeyValue{attribute.String("messaging.system", "kafka"), attribute.String("messaging.operation", "receive")},
			spanKind: oteltrace.SpanKindProducer,
			out:      "kafka.receive",
		},
		{
			in:       []attribute.KeyValue{attribute.String("messaging.system", "kafka"), attribute.String("messaging.operation", "receive")},
			spanKind: oteltrace.SpanKindConsumer,
			out:      "kafka.receive",
		},
		{
			in:       []attribute.KeyValue{attribute.String("messaging.system", "kafka"), attribute.String("messaging.operation", "receive")},
			spanKind: oteltrace.SpanKindClient,
			out:      "kafka.receive",
		},
		{
			in:       []attribute.KeyValue{attribute.String("rpc.system", "aws-api"), attribute.String("rpc.service", "Example_Method")},
			spanKind: oteltrace.SpanKindClient,
			out:      "aws." + "example_method" + ".request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("rpc.system", "aws-api"), attribute.String("rpc.service", "")},
			spanKind: oteltrace.SpanKindClient,
			out:      "aws.client.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("rpc.system", "myservice.EchoService")},
			spanKind: oteltrace.SpanKindClient,
			out:      "myservice.echoservice.client.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("rpc.system", "myservice.EchoService")},
			spanKind: oteltrace.SpanKindServer,
			out:      "myservice.echoservice.server.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("faas.invoked_provider", "some_provIDER"), attribute.String("faas.invoked_name", "some_NAME")},
			spanKind: oteltrace.SpanKindClient,
			out:      "some_provider.some_name.invoke",
		},
		{
			in:       []attribute.KeyValue{attribute.String("faas.trigger", "some_NAME")},
			spanKind: oteltrace.SpanKindServer,
			out:      "some_name.invoke",
		},
		{
			in:  []attribute.KeyValue{attribute.String("graphql.operation.type", "subscription")},
			out: "graphql.server.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("network.protocol.name", "amqp")},
			spanKind: oteltrace.SpanKindServer,
			out:      "amqp.server.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("network.protocol.name", "")},
			spanKind: oteltrace.SpanKindServer,
			out:      "server.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("network.protocol.name", "amqp")},
			spanKind: oteltrace.SpanKindClient,
			out:      "amqp.client.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("network.protocol.name", "")},
			spanKind: oteltrace.SpanKindClient,
			out:      "client.request",
		},
		{
			in:       []attribute.KeyValue{attribute.String("messaging.system", "kafka"), attribute.String("messaging.operation", "receive")},
			spanKind: oteltrace.SpanKindServer,
			out:      "kafka.receive",
		},
		{
			in:  []attribute.KeyValue{attribute.Int("operation.name", 2)},
			out: "internal",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payloads, cleanup := mockTracerProvider(t, tracer.WithEnv("test_env"), tracer.WithService("test_serv"))
	tr := otel.Tracer("")
	defer cleanup()

	for _, test := range testCases {
		t.Run("", func(t *testing.T) {
			_, sp := tr.Start(context.Background(), "some_name",
				oteltrace.WithSpanKind(test.spanKind), oteltrace.WithAttributes(test.in...))
			sp.End()

			tracer.Flush()
			p, err := waitForPayload(ctx, payloads)
			if err != nil {
				t.Fatalf(err.Error())
			}
			assert.Contains(t, p, test.out)
		})
	}
}

func TestRemapWithMultipleSetAttributes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, payloads, cleanup := mockTracerProvider(t, tracer.WithEnv("test_env"), tracer.WithService("test_serv"))
	tr := otel.Tracer("")
	defer cleanup()

	_, sp := tr.Start(context.Background(), "otel_span_name",
		oteltrace.WithSpanKind(oteltrace.SpanKindServer))

	sp.SetAttributes(attribute.String("http.request.method", "GET"))
	sp.SetAttributes(attribute.String("resource.name", "new.name"))
	sp.SetAttributes(attribute.String("operation.name", "Overriden.name"))
	sp.SetAttributes(attribute.String("service.name", "new.service.name"))
	sp.SetAttributes(attribute.String("span.type", "new.span.type"))
	sp.SetAttributes(attribute.String("analytics.event", "true"))
	sp.End()

	tracer.Flush()
	p, err := waitForPayload(ctx, payloads)
	if err != nil {
		t.Fatalf(err.Error())
	}
	assert.Contains(t, p, `"name":"overriden.name"`)
	assert.Contains(t, p, `"resource":"new.name"`)
	assert.Contains(t, p, `"service":"new.service.name"`)
	assert.Contains(t, p, `"type":"new.span.type"`)
	assert.Contains(t, p, `"_dd1.sr.eausr":1`)
}
