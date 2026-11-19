package telemetry

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// SpanKind represents the OpenTelemetry SpanKind enum values.
type SpanKind int32

const (
	SpanKindUnspecified SpanKind = 0
	SpanKindInternal    SpanKind = 1
	SpanKindServer      SpanKind = 2
	SpanKindClient      SpanKind = 3
	SpanKindProducer    SpanKind = 4
	SpanKindConsumer    SpanKind = 5
)

// StatusCode represents the OpenTelemetry StatusCode enum values.
type StatusCode int32

const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

type attrValueType uint8

const (
	attrString attrValueType = iota + 1
	attrInt
	attrFloat
	attrBool
)

// Attribute is a typed OpenTelemetry key-value attribute.
type Attribute struct {
	Key      string
	Kind     attrValueType
	StrVal   string
	IntVal   int64
	FloatVal float64
	BoolVal  bool
}

// String creates a string Attribute.
func String(k, v string) Attribute {
	return Attribute{Key: k, Kind: attrString, StrVal: v}
}

// Int creates an integer Attribute.
func Int(k string, v int) Attribute {
	return Attribute{Key: k, Kind: attrInt, IntVal: int64(v)}
}

// Int64 creates an int64 Attribute.
func Int64(k string, v int64) Attribute {
	return Attribute{Key: k, Kind: attrInt, IntVal: v}
}

// Float64 creates a float64 Attribute.
func Float64(k string, v float64) Attribute {
	return Attribute{Key: k, Kind: attrFloat, FloatVal: v}
}

// Bool creates a boolean Attribute.
func Bool(k string, v bool) Attribute {
	return Attribute{Key: k, Kind: attrBool, BoolVal: v}
}

// SpanData is an immutable completed span ready for OTLP export.
type SpanData struct {
	TraceID       [16]byte
	SpanID        [8]byte
	ParentSpanID  [8]byte
	TraceState    string
	Name          string
	Kind          SpanKind
	StartTime     time.Time
	EndTime       time.Time
	Attributes    []Attribute
	Status        StatusCode
	StatusMessage string
}

func appendBytesField(dst []byte, num protowire.Number, val []byte) []byte {
	dst = protowire.AppendTag(dst, num, protowire.BytesType)
	return protowire.AppendBytes(dst, val)
}

func appendStringField(dst []byte, num protowire.Number, val string) []byte {
	if val == "" {
		return dst
	}
	dst = protowire.AppendTag(dst, num, protowire.BytesType)
	return protowire.AppendString(dst, val)
}

func appendVarintField(dst []byte, num protowire.Number, val uint64) []byte {
	if val == 0 {
		return dst
	}
	dst = protowire.AppendTag(dst, num, protowire.VarintType)
	return protowire.AppendVarint(dst, val)
}

func appendFixed64Field(dst []byte, num protowire.Number, val uint64) []byte {
	dst = protowire.AppendTag(dst, num, protowire.Fixed64Type)
	return protowire.AppendFixed64(dst, val)
}

func encodeKeyValueProto(a Attribute) []byte {
	// opentelemetry.proto.common.v1.AnyValue:
	// 1: string_value, 2: bool_value, 3: int_value, 4: double_value
	var anyVal []byte
	switch a.Kind {
	case attrString:
		anyVal = protowire.AppendTag(anyVal, 1, protowire.BytesType)
		anyVal = protowire.AppendString(anyVal, a.StrVal)
	case attrBool:
		var b uint64
		if a.BoolVal {
			b = 1
		}
		anyVal = protowire.AppendTag(anyVal, 2, protowire.VarintType)
		anyVal = protowire.AppendVarint(anyVal, b)
	case attrInt:
		anyVal = protowire.AppendTag(anyVal, 3, protowire.VarintType)
		anyVal = protowire.AppendVarint(anyVal, uint64(a.IntVal))
	case attrFloat:
		anyVal = protowire.AppendTag(anyVal, 4, protowire.Fixed64Type)
		anyVal = protowire.AppendFixed64(anyVal, math.Float64bits(a.FloatVal))
	}
	// KeyValue: 1: key (string), 2: value (AnyValue)
	var kv []byte
	kv = appendStringField(kv, 1, a.Key)
	kv = appendBytesField(kv, 2, anyVal)
	return kv
}

func encodeSpanProto(sp SpanData) []byte {
	// opentelemetry.proto.trace.v1.Span:
	// 1: trace_id, 2: span_id, 3: trace_state, 4: parent_span_id, 5: name,
	// 6: kind, 7: start_time_unix_nano (fixed64), 8: end_time_unix_nano (fixed64),
	// 9: attributes (repeated KeyValue), 15: status (Status{2:message, 3:code})
	var b []byte
	b = appendBytesField(b, 1, sp.TraceID[:])
	b = appendBytesField(b, 2, sp.SpanID[:])
	b = appendStringField(b, 3, sp.TraceState)
	if sp.ParentSpanID != zeroSpanID {
		b = appendBytesField(b, 4, sp.ParentSpanID[:])
	}
	b = appendStringField(b, 5, sp.Name)
	b = appendVarintField(b, 6, uint64(sp.Kind))
	b = appendFixed64Field(b, 7, uint64(sp.StartTime.UnixNano()))
	b = appendFixed64Field(b, 8, uint64(sp.EndTime.UnixNano()))
	for _, a := range sp.Attributes {
		if a.Key != "" {
			b = appendBytesField(b, 9, encodeKeyValueProto(a))
		}
	}
	if sp.Status != StatusUnset || sp.StatusMessage != "" {
		var st []byte
		st = appendStringField(st, 2, sp.StatusMessage)
		st = appendVarintField(st, 3, uint64(sp.Status))
		b = appendBytesField(b, 15, st)
	}
	return b
}

// EncodeOTLPProto encodes spans as an OpenTelemetry v1 ExportTraceServiceRequest binary Protobuf payload.
func EncodeOTLPProto(serviceName, serviceVersion string, spans []SpanData) []byte {
	if serviceName == "" {
		serviceName = "flintd"
	}
	resAttrs := []Attribute{
		String("service.name", serviceName),
		String("telemetry.sdk.name", "flint-telemetry"),
		String("telemetry.sdk.language", "go"),
	}
	if serviceVersion != "" {
		resAttrs = append(resAttrs, String("service.version", serviceVersion))
	}
	var resourceMsg []byte
	for _, ra := range resAttrs {
		resourceMsg = appendBytesField(resourceMsg, 1, encodeKeyValueProto(ra))
	}

	var scopeMsg []byte
	scopeMsg = appendStringField(scopeMsg, 1, "github.com/flint-fhir/flint/pkg/telemetry")
	if serviceVersion != "" {
		scopeMsg = appendStringField(scopeMsg, 2, serviceVersion)
	}

	var scopeSpansMsg []byte
	scopeSpansMsg = appendBytesField(scopeSpansMsg, 1, scopeMsg)
	for _, sp := range spans {
		scopeSpansMsg = appendBytesField(scopeSpansMsg, 2, encodeSpanProto(sp))
	}

	var resourceSpansMsg []byte
	resourceSpansMsg = appendBytesField(resourceSpansMsg, 1, resourceMsg)
	resourceSpansMsg = appendBytesField(resourceSpansMsg, 2, scopeSpansMsg)

	var reqMsg []byte
	reqMsg = appendBytesField(reqMsg, 1, resourceSpansMsg)
	return reqMsg
}

type otlpJSONKeyValue struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func toOTLPJSONAttrs(attrs []Attribute) []otlpJSONKeyValue {
	out := make([]otlpJSONKeyValue, 0, len(attrs))
	for _, a := range attrs {
		if a.Key == "" {
			continue
		}
		val := make(map[string]any, 1)
		switch a.Kind {
		case attrString:
			val["stringValue"] = a.StrVal
		case attrBool:
			val["boolValue"] = a.BoolVal
		case attrInt:
			val["intValue"] = strconv.FormatInt(a.IntVal, 10)
		case attrFloat:
			val["doubleValue"] = a.FloatVal
		}
		out = append(out, otlpJSONKeyValue{Key: a.Key, Value: val})
	}
	return out
}

// EncodeOTLPJSON encodes spans as an OpenTelemetry v1 ExportTraceServiceRequest JSON payload.
func EncodeOTLPJSON(serviceName, serviceVersion string, spans []SpanData) ([]byte, error) {
	if serviceName == "" {
		serviceName = "flintd"
	}
	resAttrs := []Attribute{
		String("service.name", serviceName),
		String("telemetry.sdk.name", "flint-telemetry"),
		String("telemetry.sdk.language", "go"),
	}
	if serviceVersion != "" {
		resAttrs = append(resAttrs, String("service.version", serviceVersion))
	}

	jsonSpans := make([]map[string]any, 0, len(spans))
	for _, sp := range spans {
		m := map[string]any{
			"traceId":           hex.EncodeToString(sp.TraceID[:]),
			"spanId":            hex.EncodeToString(sp.SpanID[:]),
			"name":              sp.Name,
			"kind":              int32(sp.Kind),
			"startTimeUnixNano": strconv.FormatInt(sp.StartTime.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(sp.EndTime.UnixNano(), 10),
			"attributes":        toOTLPJSONAttrs(sp.Attributes),
		}
		if sp.ParentSpanID != zeroSpanID {
			m["parentSpanId"] = hex.EncodeToString(sp.ParentSpanID[:])
		}
		if sp.TraceState != "" {
			m["traceState"] = sp.TraceState
		}
		if sp.Status != StatusUnset || sp.StatusMessage != "" {
			m["status"] = map[string]any{
				"code":    int32(sp.Status),
				"message": sp.StatusMessage,
			}
		}
		jsonSpans = append(jsonSpans, m)
	}

	payload := map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": toOTLPJSONAttrs(resAttrs),
				},
				"scopeSpans": []map[string]any{
					{
						"scope": map[string]any{
							"name":    "github.com/flint-fhir/flint/pkg/telemetry",
							"version": serviceVersion,
						},
						"spans": jsonSpans,
					},
				},
			},
		},
	}
	return json.Marshal(payload)
}
