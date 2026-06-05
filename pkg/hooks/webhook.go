package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const DefaultWebhookTimeout = 5 * time.Second

type Hook interface {
	Match(EventType) bool
	Deliver(Event) error
}

type WebhookHook struct {
	URL    string
	Events []EventType
	client *http.Client
}

func NewWebhookHook(url string, events []EventType, timeout time.Duration) *WebhookHook {
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	return &WebhookHook{URL: url, Events: events, client: &http.Client{Timeout: timeout}}
}

func (h *WebhookHook) Match(t EventType) bool {
	for _, event := range h.Events {
		if event == t {
			return true
		}
	}
	return false
}

func (h *WebhookHook) Deliver(e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	slog.Info("webhook response", "url", h.URL, "status", resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s returned status %d", h.URL, resp.StatusCode)
	}
	return nil
}
