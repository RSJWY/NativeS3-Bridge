package controlproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 遥测契约:字段缺失、合法零值、非法 observed_at 必须可区分,且新旧对端
// 互相兼容(旧 Node 不发字段 / 旧 Panel 忽略新字段)。

func int64Ptr(v int64) *int64 { return &v }

func TestHeartbeatTelemetryOmittedWhenFieldsAbsent(t *testing.T) {
	// 旧版节点:只上报 applied_version,JSON 不含遥测字段。
	raw := `{"applied_version":7}`
	var hb HeartbeatPayload
	if err := json.Unmarshal([]byte(raw), &hb); err != nil {
		t.Fatal(err)
	}
	if hb.UsedBytesTotal != nil || hb.ObjectCount != nil || hb.ObservedAt != "" {
		t.Fatalf("expected absent telemetry fields, got %+v", hb)
	}
	if _, ok := hb.Telemetry(); ok {
		t.Fatal("legacy heartbeat must not produce telemetry")
	}
	// 编码方向同样不得带出字段。
	encoded, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "used_bytes_total") || strings.Contains(string(encoded), "object_count") ||
		strings.Contains(string(encoded), "observed_at") {
		t.Fatalf("omitempty leaked absent telemetry fields: %s", encoded)
	}
}

func TestHeartbeatTelemetryExplicitZeroIsComplete(t *testing.T) {
	// 合法的 0 字节 / 0 对象节点:显式 0 必须是完整遥测,不是"未上报"。
	observed := "2026-08-15T12:00:00Z"
	raw := `{"applied_version":1,"used_bytes_total":0,"object_count":0,"observed_at":"` + observed + `"}`
	var hb HeartbeatPayload
	if err := json.Unmarshal([]byte(raw), &hb); err != nil {
		t.Fatal(err)
	}
	telemetry, ok := hb.Telemetry()
	if !ok {
		t.Fatal("explicit zero snapshot must be valid telemetry")
	}
	if telemetry.UsedBytesTotal != 0 || telemetry.ObjectCount != 0 {
		t.Fatalf("zero snapshot corrupted: %+v", telemetry)
	}
	want, err := time.Parse(time.RFC3339, observed)
	if err != nil {
		t.Fatal(err)
	}
	if !telemetry.ObservedAt.Equal(want) {
		t.Fatalf("observed_at = %v, want %v", telemetry.ObservedAt, want)
	}
	if telemetry.ObservedAt.Location() != time.UTC {
		t.Fatalf("observed_at must be normalized to UTC, got %v", telemetry.ObservedAt.Location())
	}
}

func TestHeartbeatTelemetryPartialOrMalformedIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"only bytes", `{"applied_version":1,"used_bytes_total":10}`},
		{"only count", `{"applied_version":1,"object_count":3}`},
		{"missing observed_at", `{"applied_version":1,"used_bytes_total":10,"object_count":3}`},
		{"empty observed_at", `{"applied_version":1,"used_bytes_total":10,"object_count":3,"observed_at":""}`},
		{"malformed observed_at", `{"applied_version":1,"used_bytes_total":10,"object_count":3,"observed_at":"yesterday"}`},
		{"non-numeric bytes", `{"applied_version":1,"used_bytes_total":"10","object_count":3,"observed_at":"2026-08-15T12:00:00Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hb HeartbeatPayload
			if err := json.Unmarshal([]byte(tc.raw), &hb); err != nil {
				// 解码失败同样按不可用处理。
				return
			}
			if _, ok := hb.Telemetry(); ok {
				t.Fatalf("payload %s must not be treated as valid telemetry", tc.raw)
			}
		})
	}
}

func TestHeartbeatTelemetryEnvelopeRoundTrip(t *testing.T) {
	observed := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	sent := HeartbeatPayload{
		AppliedVersion: 4,
		UsedBytesTotal: int64Ptr(1234),
		ObjectCount:    int64Ptr(7),
		ObservedAt:     observed.Format(time.RFC3339Nano),
	}
	env, err := NewEnvelope(TypeHeartbeat, "", sent)
	if err != nil {
		t.Fatal(err)
	}
	var received HeartbeatPayload
	if err := env.DecodePayload(&received); err != nil {
		t.Fatal(err)
	}
	telemetry, ok := received.Telemetry()
	if !ok {
		t.Fatal("round-tripped telemetry must stay valid")
	}
	if telemetry.UsedBytesTotal != 1234 || telemetry.ObjectCount != 7 || !telemetry.ObservedAt.Equal(observed) {
		t.Fatalf("telemetry corrupted: %+v", telemetry)
	}
}
