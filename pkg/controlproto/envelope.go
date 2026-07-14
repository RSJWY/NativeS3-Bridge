// Package controlproto defines the shared control-plane wire protocol between
// the panel (cmd/panel) and node agents (cmd/node). The transport is WebSocket
// + JSON over mTLS; this package owns only the message contract: the envelope,
// message type constants, protocol version negotiation, and payload schemas.
//
// Both the panel and node binaries import this package, so it must stay free of
// panel- or node-specific dependencies. New payload fields must be added in a
// backward-compatible way (unknown fields are ignored on decode); breaking
// changes require bumping ProtocolVersion.
package controlproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Protocol version constants. ProtocolVersion is the version this build speaks.
// MinCompatibleVersion is the oldest peer version this build can interoperate
// with. Version negotiation happens in the hello/hello_ack handshake.
const (
	ProtocolVersion      = 1
	MinCompatibleVersion = 1
)

// MessageType enumerates the envelope discriminators. Unknown types must be
// treated as a protocol error by the receiver, not silently dropped.
type MessageType string

const (
	TypeHello        MessageType = "hello"
	TypeHelloAck     MessageType = "hello_ack"
	TypeHeartbeat    MessageType = "heartbeat"
	TypeHeartbeatAck MessageType = "heartbeat_ack"
	TypeDesiredState MessageType = "desired_state"
	TypeAck          MessageType = "ack"
	TypeTask         MessageType = "task"
	TypeTaskResult   MessageType = "task_result"
	TypeError        MessageType = "error"

	// Migration (in-place import) message types. The panel asks a freshly
	// connected, not-yet-imported node to report its existing local business
	// config; the node replies read-only. No config is written to the node until
	// an admin confirms the import on the panel (design §8.3).
	TypeImportRequest MessageType = "import_request"
	TypeImportReport  MessageType = "import_report"
)

// Envelope is the uniform wrapper for every control-plane message. Type drives
// dispatch; ID correlates tasks and their results (heartbeats may omit it).
// Payload carries the type-specific structure and is decoded lazily so that
// unknown/extra fields inside a known payload are ignored for forward
// compatibility.
type Envelope struct {
	Type    MessageType     `json:"type"`
	Version int             `json:"version"`
	ID      string          `json:"id,omitempty"`
	TS      time.Time       `json:"ts"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope builds an envelope for the given type, marshalling payload into
// the Payload field. A nil payload produces an envelope with no payload.
func NewEnvelope(msgType MessageType, id string, payload any) (Envelope, error) {
	env := Envelope{
		Type:    msgType,
		Version: ProtocolVersion,
		ID:      id,
		TS:      time.Now().UTC(),
	}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return Envelope{}, fmt.Errorf("marshal %s payload: %w", msgType, err)
		}
		env.Payload = raw
	}
	return env, nil
}

// Encode serializes the envelope to JSON bytes for transmission.
func (e Envelope) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodeEnvelope parses raw wire bytes into an Envelope without interpreting the
// payload. It rejects empty input and envelopes with no type.
func DecodeEnvelope(data []byte) (Envelope, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Envelope{}, fmt.Errorf("empty message")
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, fmt.Errorf("envelope missing type")
	}
	return env, nil
}

// DecodePayload unmarshals the envelope payload into out. Unknown fields are
// ignored (forward compatibility): decoding uses the standard library default,
// which skips fields absent from the target struct. A nil/empty payload leaves
// out untouched.
func (e Envelope) DecodePayload(out any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Payload, out); err != nil {
		return fmt.Errorf("decode %s payload: %w", e.Type, err)
	}
	return nil
}
