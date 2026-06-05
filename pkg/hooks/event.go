package hooks

type EventType string

const (
	ObjectCreated EventType = "ObjectCreated"
	ObjectDeleted EventType = "ObjectDeleted"
)

type Event struct {
	Type         EventType         `json:"type"`
	Bucket       string            `json:"bucket"`
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	CredentialID uint              `json:"credential_id"`
	Timestamp    string            `json:"timestamp"`
}
