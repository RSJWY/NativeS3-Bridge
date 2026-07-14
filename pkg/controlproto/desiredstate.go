package controlproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// DesiredState is the panel-authoritative business configuration pushed to a
// node: credentials, buckets, ACLs, quotas, webhooks, and rate-limit policy.
// The node persists the last successfully applied copy and serves S3 from it
// even when the panel is unreachable.
//
// This is a full snapshot, not a delta. The content hash (see ContentHash) is
// computed over a canonical form so the panel and node can independently detect
// drift by comparing hashes for the same version.
type DesiredState struct {
	Credentials []DesiredCredential `json:"credentials"`
	Buckets     []DesiredBucket     `json:"buckets"`
	Webhooks    []DesiredWebhook    `json:"webhooks"`
	RateLimit   *DesiredRateLimit   `json:"rate_limit,omitempty"`
}

// DesiredCredential mirrors the node-level credential record. SecretKey is the
// plaintext S3 secret required for SigV4 verification; it travels only over the
// established mTLS channel and is stored plaintext only in the node-local DB.
type DesiredCredential struct {
	AccessKey  string `json:"access_key"`
	SecretKey  string `json:"secret_key"`
	Name       string `json:"name,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Status     string `json:"status"`
	QuotaBytes int64  `json:"quota_bytes"`
}

// DesiredBucket mirrors the node-level bucket record.
type DesiredBucket struct {
	Name string `json:"name"`
	ACL  string `json:"acl"`
}

// DesiredWebhook mirrors the node-level hook configuration.
type DesiredWebhook struct {
	URL     string `json:"url"`
	Events  string `json:"events"`
	Enabled bool   `json:"enabled"`
}

// DesiredRateLimit mirrors the node anonymous rate-limit policy.
type DesiredRateLimit struct {
	AnonymousRPS   float64 `json:"anonymous_rps"`
	AnonymousBurst int     `json:"anonymous_burst"`
	TrustForwarded bool    `json:"trust_forwarded"`
}

// ContentHash returns a stable SHA-256 hex digest of the desired state. The
// hash is computed over a canonicalized copy (slices sorted by natural key) so
// that logically-equal states always hash identically regardless of the order
// in which the panel emitted the entries. This is the drift-detection primitive
// shared by both sides.
func (d DesiredState) ContentHash() string {
	canonical := d.canonical()
	// json.Marshal emits struct fields in declaration order and map keys sorted,
	// so a canonicalized struct produces deterministic bytes.
	raw, err := json.Marshal(canonical)
	if err != nil {
		// Marshalling a plain struct of strings/numbers cannot fail; fall back to
		// an empty-state hash rather than panicking.
		raw = []byte("{}")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// canonical returns a copy of the desired state with all slices sorted by their
// natural unique key so ordering does not affect the content hash.
func (d DesiredState) canonical() DesiredState {
	out := DesiredState{
		Credentials: make([]DesiredCredential, len(d.Credentials)),
		Buckets:     make([]DesiredBucket, len(d.Buckets)),
		Webhooks:    make([]DesiredWebhook, len(d.Webhooks)),
		RateLimit:   d.RateLimit,
	}
	copy(out.Credentials, d.Credentials)
	copy(out.Buckets, d.Buckets)
	copy(out.Webhooks, d.Webhooks)

	sort.Slice(out.Credentials, func(i, j int) bool {
		return out.Credentials[i].AccessKey < out.Credentials[j].AccessKey
	})
	sort.Slice(out.Buckets, func(i, j int) bool {
		return out.Buckets[i].Name < out.Buckets[j].Name
	})
	sort.Slice(out.Webhooks, func(i, j int) bool {
		if out.Webhooks[i].URL != out.Webhooks[j].URL {
			return out.Webhooks[i].URL < out.Webhooks[j].URL
		}
		return out.Webhooks[i].Events < out.Webhooks[j].Events
	})
	return out
}
