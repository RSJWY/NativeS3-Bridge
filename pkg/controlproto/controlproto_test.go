package controlproto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	hello := HelloPayload{
		ProtocolVersion: ProtocolVersion,
		NodeID:          "node-1",
		AgentVersion:    "test",
		AppliedVersion:  7,
		ContentHash:     "abc",
		Capabilities:    []string{CapabilityAuthoritativeConfigV1},
	}
	env, err := NewEnvelope(TypeHello, "req-1", hello)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.Version != ProtocolVersion {
		t.Fatalf("version = %d, want %d", env.Version, ProtocolVersion)
	}
	if env.TS.IsZero() {
		t.Fatal("timestamp should be set")
	}

	raw, err := env.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	decoded, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if decoded.Type != TypeHello {
		t.Fatalf("type = %q, want %q", decoded.Type, TypeHello)
	}
	if decoded.ID != "req-1" {
		t.Fatalf("id = %q, want req-1", decoded.ID)
	}

	var out HelloPayload
	if err := decoded.DecodePayload(&out); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !reflect.DeepEqual(out, hello) {
		t.Fatalf("payload = %+v, want %+v", out, hello)
	}
}

func TestNewEnvelopeNilPayload(t *testing.T) {
	env, err := NewEnvelope(TypeHeartbeat, "", nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if len(env.Payload) != 0 {
		t.Fatalf("payload should be empty, got %q", env.Payload)
	}
	// DecodePayload on empty payload must be a no-op, not an error.
	var hb HeartbeatPayload
	if err := env.DecodePayload(&hb); err != nil {
		t.Fatalf("DecodePayload on empty: %v", err)
	}
}

func TestDecodeEnvelopeRejectsBadInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":        []byte(""),
		"whitespace":   []byte("   \n"),
		"missing type": []byte(`{"version":1,"ts":"2026-07-14T00:00:00Z"}`),
		"not json":     []byte("{"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEnvelope(data); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestDecodePayloadIgnoresUnknownFields(t *testing.T) {
	// Simulate a newer peer that adds an extra field to a known payload. Older
	// builds must decode successfully and ignore the unknown field.
	raw := []byte(`{
		"type":"hello",
		"version":1,
		"ts":"2026-07-14T00:00:00Z",
		"payload":{"node_id":"n1","applied_version":3,"future_field":"ignored","nested":{"x":1}}
	}`)
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	var hello HelloPayload
	if err := env.DecodePayload(&hello); err != nil {
		t.Fatalf("DecodePayload with unknown fields: %v", err)
	}
	if hello.NodeID != "n1" || hello.AppliedVersion != 3 {
		t.Fatalf("decoded = %+v", hello)
	}
}

func TestNegotiateVersion(t *testing.T) {
	cases := []struct {
		name    string
		peer    int
		want    int
		wantErr bool
	}{
		{"same version", ProtocolVersion, ProtocolVersion, false},
		{"peer newer", ProtocolVersion + 5, ProtocolVersion, false},
		{"peer zero", 0, 0, true},
		{"peer negative", -1, 0, true},
		{"peer below floor", MinCompatibleVersion - 1, 0, MinCompatibleVersion-1 < MinCompatibleVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NegotiateVersion(tc.peer)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for peer=%d", tc.peer)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("negotiated = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestIsCompatible(t *testing.T) {
	if !IsCompatible(ProtocolVersion) {
		t.Fatal("current version must be compatible")
	}
	if IsCompatible(MinCompatibleVersion - 1) {
		t.Fatal("below floor must be incompatible")
	}
	if IsCompatible(ProtocolVersion + 1) {
		t.Fatal("above current must be incompatible")
	}
}

func TestContentHashStableAcrossOrdering(t *testing.T) {
	a := DesiredState{
		Credentials: []DesiredCredential{
			{AccessKey: "AK2", SecretKey: "s2", Status: "enabled"},
			{AccessKey: "AK1", SecretKey: "s1", Status: "enabled"},
		},
		Buckets: []DesiredBucket{
			{Name: "b2", ACL: "private"},
			{Name: "b1", ACL: "public-read"},
		},
		Webhooks: []DesiredWebhook{
			{URL: "https://z", Events: "put", Enabled: true},
			{URL: "https://a", Events: "delete", Enabled: false},
		},
	}
	// Same logical content, different slice ordering.
	b := DesiredState{
		Credentials: []DesiredCredential{
			{AccessKey: "AK1", SecretKey: "s1", Status: "enabled"},
			{AccessKey: "AK2", SecretKey: "s2", Status: "enabled"},
		},
		Buckets: []DesiredBucket{
			{Name: "b1", ACL: "public-read"},
			{Name: "b2", ACL: "private"},
		},
		Webhooks: []DesiredWebhook{
			{URL: "https://a", Events: "delete", Enabled: false},
			{URL: "https://z", Events: "put", Enabled: true},
		},
	}
	if a.ContentHash() != b.ContentHash() {
		t.Fatalf("hash should be order-independent:\n a=%s\n b=%s", a.ContentHash(), b.ContentHash())
	}
}

func TestContentHashChangesOnContentChange(t *testing.T) {
	base := DesiredState{
		Credentials: []DesiredCredential{{AccessKey: "AK1", SecretKey: "s1", Status: "enabled"}},
	}
	changed := DesiredState{
		Credentials: []DesiredCredential{{AccessKey: "AK1", SecretKey: "s1-rotated", Status: "enabled"}},
	}
	if base.ContentHash() == changed.ContentHash() {
		t.Fatal("hash must change when a secret rotates")
	}
}

func TestContentHashEmptyIsStable(t *testing.T) {
	var a, b DesiredState
	if a.ContentHash() != b.ContentHash() {
		t.Fatal("empty desired states must hash equally")
	}
	if a.ContentHash() == "" {
		t.Fatal("hash must not be empty")
	}
}

func TestDesiredStatePayloadRoundTrip(t *testing.T) {
	ds := DesiredState{
		Credentials: []DesiredCredential{{AccessKey: "AK1", SecretKey: "s1", Status: "enabled", QuotaBytes: 1024}},
		Buckets:     []DesiredBucket{{Name: "b1", ACL: "private"}},
		RateLimit:   &DesiredRateLimit{AnonymousRPS: 10, AnonymousBurst: 20},
	}
	payload := DesiredStatePayload{Version: 3, ContentHash: ds.ContentHash(), Content: ds}
	env, err := NewEnvelope(TypeDesiredState, "d-1", payload)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	raw, err := env.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	var out DesiredStatePayload
	if err := decoded.DecodePayload(&out); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if out.Version != 3 {
		t.Fatalf("version = %d, want 3", out.Version)
	}
	if out.Content.ContentHash() != ds.ContentHash() {
		t.Fatal("content hash mismatch after round trip")
	}
	if out.ContentHash != out.Content.ContentHash() {
		t.Fatal("declared hash must match recomputed hash")
	}
}

func TestEnvelopeJSONShape(t *testing.T) {
	// Guard the wire field names against accidental renames.
	env, err := NewEnvelope(TypeTask, "t-1", TaskPayload{TaskID: "t-1", Type: TaskLogQuery})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	raw, err := env.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"type", "version", "id", "ts", "payload"} {
		if _, ok := generic[key]; !ok {
			t.Fatalf("envelope missing wire key %q", key)
		}
	}
}
