package newrelic

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"github.com/newrelic/go-agent/v3/internal"
	v1 "github.com/newrelic/go-agent/v3/internal/com_newrelic_trace_v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

type traceBox struct {
	messages chan *internal.SpanEvent
}

const (
	apiKeyMetadataKey        = "api_key"
	traceboxMessageQueueSize = 1000
)

var (
	traceBoxBackoffStrategy = []time.Duration{
		15 * time.Second,
		15 * time.Second,
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		300 * time.Second,
	}
)

func getTraceBoxBackoff(attempt int) time.Duration {
	if attempt < len(traceBoxBackoffStrategy) {
		return traceBoxBackoffStrategy[attempt]
	}
	return traceBoxBackoffStrategy[len(traceBoxBackoffStrategy)-1]
}

func newTraceBox(endpoint, apiKey string, lg Logger) (*traceBox, error) {
	messages := make(chan *internal.SpanEvent, traceboxMessageQueueSize)

	go func() {
		attempts := 0
		for {
			err := spawnConnection(endpoint, apiKey, lg, messages)
			if nil != err {
				// TODO: Maybe decide if a reconnect should be
				// tried.
				fmt.Println(err)
			}
			time.Sleep(getTraceBoxBackoff(attempts))
			attempts++

		}
	}()

	return &traceBox{messages: messages}, nil
}

func spawnConnection(endpoint, apiKey string, lg Logger, messages <-chan *internal.SpanEvent) error {

	responseError := make(chan error, 1)

	conn, err := grpc.Dial(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	if nil != err {
		return fmt.Errorf("unable to dial grpc endpoint %s: %v", endpoint, err)
	}

	serviceClient := v1.NewIngestServiceClient(conn)

	spanClient, err := serviceClient.RecordSpan(metadata.AppendToOutgoingContext(context.Background(), "api_key", apiKey))
	if nil != err {
		return fmt.Errorf("unable to create span client: %v", err)
	}

	go func() {
		for {
			status, err := spanClient.Recv()
			if nil != err {
				lg.Error("trace box response error", map[string]interface{}{
					"err": err.Error(),
				})
				responseError <- err
				return
			}
			lg.Debug("trace box response", map[string]interface{}{
				"messages_seen": status.GetMessagesSeen(),
			})
		}
	}()

	for {
		var err error
		var event *internal.SpanEvent
		select {
		case err = <-responseError:
		case event = <-messages:
		}
		if nil != err {
			lg.Debug("trace box sender received response error", map[string]interface{}{
				"err": err.Error(),
			})
			break
		}
		span := transformEvent(event)
		lg.Debug("sending span to trace box", map[string]interface{}{
			"name": event.Name,
		})
		err = spanClient.Send(span)
		if nil != err {
			lg.Debug("trace box sender send error", map[string]interface{}{
				"err": err.Error(),
			})
			break
		}
	}

	lg.Debug("closing trace box sender", map[string]interface{}{})
	err = spanClient.CloseSend()
	if nil != err {
		lg.Debug("error closing trace box sender", map[string]interface{}{
			"err": err.Error(),
		})
	}

	return nil
}

func mtbString(s string) *v1.AttributeValue {
	return &v1.AttributeValue{Value: &v1.AttributeValue_StringValue{StringValue: s}}
}

func mtbBool(b bool) *v1.AttributeValue {
	return &v1.AttributeValue{Value: &v1.AttributeValue_BoolValue{BoolValue: b}}
}

func mtbInt(x int64) *v1.AttributeValue {
	return &v1.AttributeValue{Value: &v1.AttributeValue_IntValue{IntValue: x}}
}

func mtbDouble(x float64) *v1.AttributeValue {
	return &v1.AttributeValue{Value: &v1.AttributeValue_DoubleValue{DoubleValue: x}}
}

func transformEvent(e *internal.SpanEvent) *v1.Span {
	span := &v1.Span{
		TraceId:         e.TraceID,
		Intrinsics:      make(map[string]*v1.AttributeValue),
		UserAttributes:  make(map[string]*v1.AttributeValue),
		AgentAttributes: make(map[string]*v1.AttributeValue),
	}

	span.Intrinsics["type"] = mtbString("Span")
	span.Intrinsics["traceId"] = mtbString(e.TraceID)
	span.Intrinsics["guid"] = mtbString(e.GUID)
	if "" != e.ParentID {
		span.Intrinsics["parentId"] = mtbString(e.ParentID)
	}
	span.Intrinsics["transactionId"] = mtbString(e.TransactionID)
	span.Intrinsics["sampled"] = mtbBool(e.Sampled)
	span.Intrinsics["priority"] = mtbDouble(float64(e.Priority.Float32()))
	span.Intrinsics["timestamp"] = mtbInt(e.Timestamp.UnixNano() / (1000 * 1000)) // in milliseconds
	span.Intrinsics["duration"] = mtbDouble(e.Duration.Seconds())
	span.Intrinsics["name"] = mtbString(e.Name)
	span.Intrinsics["category"] = mtbString(string(e.Category))
	if e.IsEntrypoint {
		span.Intrinsics["nr.entryPoint"] = mtbBool(true)
	}
	if e.Component != "" {
		span.Intrinsics["component"] = mtbString(e.Component)
	}
	if e.Kind != "" {
		span.Intrinsics["span.kind"] = mtbString(e.Kind)
	}
	if "" != e.TrustedParentID {
		span.Intrinsics["trustedParentId"] = mtbString(e.TrustedParentID)
	}
	if "" != e.TracingVendors {
		span.Intrinsics["tracingVendors"] = mtbString(e.TracingVendors)
	}

	for key, val := range e.Attributes {
		// This assumes all values are string types.
		// TODO: Future-proof this!
		b := bytes.Buffer{}
		val.WriteJSON(&b)
		s := strings.Trim(b.String(), `"`)
		span.AgentAttributes[key.String()] = mtbString(s)
	}

	return span
}

// func (tb *traceBox) sendSpans(events []*internal.SpanEvent) {
// 	for _, e := range events {
// 		span := transformEvent(e)
// 		fmt.Println("sending span", e.Name)
// 		err := tb.spanClient.Send(span)
// 		if nil != err {
// 			// TODO: Deal with this.
// 			fmt.Println("spanClient.Send error", err.Error())
// 		}
// 	}
// }

func (tb *traceBox) consumeSpan(span *internal.SpanEvent) bool {
	select {
	case tb.messages <- span:
		return true
	default:
		return false
	}
}
