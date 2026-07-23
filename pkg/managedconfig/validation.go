// Package managedconfig owns validation shared by the panel draft API and the
// node desired-state executor. Keeping these rules in one place prevents the
// control-plane boundary from accepting a value that the data plane later
// cannot apply safely.
package managedconfig

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/hooks"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

const MaxCredentialNameRunes = 128

var (
	ErrInvalidCredential = errors.New("invalid credential")
	ErrInvalidWebhook    = errors.New("invalid webhook")
	ErrInvalidRateLimit  = errors.New("invalid rate limit")
)

// NormalizeCredentialStatus applies the wire-compatible default used by older
// desired-state payloads.
func NormalizeCredentialStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "enabled"
	}
	return status
}

func ValidateCredentialFields(name, bucket, status string, quotaBytes int64) error {
	if utf8.RuneCountInString(strings.TrimSpace(name)) > MaxCredentialNameRunes {
		return fmt.Errorf("%w: name must be at most %d characters", ErrInvalidCredential, MaxCredentialNameRunes)
	}
	if bucket != "" {
		if err := storage.ValidateBucketName(bucket); err != nil {
			return fmt.Errorf("%w: invalid bucket name", ErrInvalidCredential)
		}
	}
	status = NormalizeCredentialStatus(status)
	if status != "enabled" && status != "disabled" {
		return fmt.Errorf("%w: status must be enabled or disabled", ErrInvalidCredential)
	}
	if quotaBytes < 0 {
		return fmt.Errorf("%w: quota_bytes must be >= 0", ErrInvalidCredential)
	}
	return nil
}

// CanonicalWebhookEvents validates the admin-facing event array and returns a
// stable comma-separated representation for persistence and desired snapshots.
func CanonicalWebhookEvents(events []string) (string, []string, error) {
	seen := make(map[string]struct{}, len(events))
	canonical := make([]string, 0, len(events))
	for _, raw := range events {
		event := strings.TrimSpace(raw)
		switch hooks.EventType(event) {
		case hooks.ObjectCreated, hooks.ObjectDeleted:
		default:
			return "", nil, fmt.Errorf("%w: unsupported event %q", ErrInvalidWebhook, event)
		}
		if _, ok := seen[event]; ok {
			continue
		}
		seen[event] = struct{}{}
		canonical = append(canonical, event)
	}
	if len(canonical) == 0 {
		return "", nil, fmt.Errorf("%w: at least one event is required", ErrInvalidWebhook)
	}
	sort.Strings(canonical)
	return strings.Join(canonical, ","), canonical, nil
}

func ParseWebhookEvents(events string) ([]string, error) {
	parts := strings.Split(events, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, raw := range parts {
		event := strings.TrimSpace(raw)
		switch hooks.EventType(event) {
		case hooks.ObjectCreated, hooks.ObjectDeleted:
		default:
			return nil, fmt.Errorf("%w: unsupported event %q", ErrInvalidWebhook, event)
		}
		if _, ok := seen[event]; ok {
			return nil, fmt.Errorf("%w: duplicate event %q", ErrInvalidWebhook, event)
		}
		seen[event] = struct{}{}
		out = append(out, event)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: at least one event is required", ErrInvalidWebhook)
	}
	return out, nil
}

func ValidateWebhook(rawURL, events string) error {
	if len(rawURL) > 512 {
		return fmt.Errorf("%w: url is too long", ErrInvalidWebhook)
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%w: url must be an absolute http or https URL", ErrInvalidWebhook)
	}
	if _, err := ParseWebhookEvents(events); err != nil {
		return err
	}
	return nil
}

func ValidateRateLimit(rps float64, burst int) error {
	if math.IsNaN(rps) || math.IsInf(rps, 0) || rps <= 0 {
		return fmt.Errorf("%w: anonymous_rps must be > 0", ErrInvalidRateLimit)
	}
	if burst <= 0 {
		return fmt.Errorf("%w: anonymous_burst must be > 0", ErrInvalidRateLimit)
	}
	return nil
}

// ValidateDesiredState rejects an invalid full snapshot before the node starts
// any filesystem or database mutation.
func ValidateDesiredState(state controlproto.DesiredState) error {
	buckets := make(map[string]struct{}, len(state.Buckets))
	for _, bucket := range state.Buckets {
		if err := storage.ValidateBucketName(bucket.Name); err != nil {
			return fmt.Errorf("invalid bucket %q", bucket.Name)
		}
		if bucket.ACL != storage.ACLPrivate && bucket.ACL != storage.ACLPublicRead {
			return fmt.Errorf("invalid acl for bucket %q", bucket.Name)
		}
		if _, exists := buckets[bucket.Name]; exists {
			return fmt.Errorf("duplicate bucket %q", bucket.Name)
		}
		buckets[bucket.Name] = struct{}{}
	}

	credentials := make(map[string]struct{}, len(state.Credentials))
	for _, credential := range state.Credentials {
		if credential.AccessKey == "" || len(credential.AccessKey) > 128 {
			return fmt.Errorf("%w: access_key is required and must be at most 128 bytes", ErrInvalidCredential)
		}
		if credential.SecretKey == "" || len(credential.SecretKey) > 256 {
			return fmt.Errorf("%w: secret_key is required and must be at most 256 bytes", ErrInvalidCredential)
		}
		if _, exists := credentials[credential.AccessKey]; exists {
			return fmt.Errorf("%w: duplicate access_key %q", ErrInvalidCredential, credential.AccessKey)
		}
		credentials[credential.AccessKey] = struct{}{}
		status := NormalizeCredentialStatus(credential.Status)
		if err := ValidateCredentialFields(credential.Name, credential.Bucket, status, credential.QuotaBytes); err != nil {
			return err
		}
		if credential.Bucket != "" {
			if _, exists := buckets[credential.Bucket]; !exists {
				return fmt.Errorf("%w: credential %q references undeclared bucket %q", ErrInvalidCredential, credential.AccessKey, credential.Bucket)
			}
		}
	}

	webhooks := make(map[string]struct{}, len(state.Webhooks))
	for _, webhook := range state.Webhooks {
		if err := ValidateWebhook(webhook.URL, webhook.Events); err != nil {
			return err
		}
		key := webhook.URL + "\x00" + webhook.Events
		if _, exists := webhooks[key]; exists {
			return fmt.Errorf("%w: duplicate webhook", ErrInvalidWebhook)
		}
		webhooks[key] = struct{}{}
	}

	if state.RateLimit != nil {
		if err := ValidateRateLimit(state.RateLimit.AnonymousRPS, state.RateLimit.AnonymousBurst); err != nil {
			return err
		}
	}
	return nil
}
