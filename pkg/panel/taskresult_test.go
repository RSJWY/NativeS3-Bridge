package panel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

func TestSanitizeTaskResultBoundsAndRedactsRemoteLogs(t *testing.T) {
	entries := make([]controlproto.TaskLogEntry, controlproto.MaxLogQueryLimit+1)
	lines := make([]string, len(entries))
	for index := range entries {
		entries[index] = controlproto.TaskLogEntry{
			Level: "info", Msg: strings.Repeat("m", 1024),
			Attrs: map[string]string{"node": "7", "authorization_token": "must-not-leak"},
		}
		lines[index] = strings.Repeat("l", 1024)
	}
	result := sanitizeTaskResult(controlproto.TaskLogQuery, controlproto.TaskResult{
		LogEntries: entries, LogLines: lines, LogSource: "untrusted-file",
	})
	if result.LogSource != "ring" || !result.LogTruncated {
		t.Fatalf("result metadata = %+v", result)
	}
	if len(result.LogEntries) > controlproto.MaxLogQueryLimit || len(result.LogLines) > controlproto.MaxLogQueryLimit {
		t.Fatalf("result counts = %d/%d", len(result.LogEntries), len(result.LogLines))
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > controlproto.MaxLogQueryResultBytes || strings.Contains(string(encoded), "must-not-leak") {
		t.Fatalf("unsafe result: bytes=%d leaked=%v", len(encoded), strings.Contains(string(encoded), "must-not-leak"))
	}
}

func TestSanitizeTaskErrorAllowsKnownMessagesOnly(t *testing.T) {
	if got := sanitizeTaskError(controlproto.TaskLogQuery, "log since must be RFC3339"); got != "log since must be RFC3339" {
		t.Fatalf("known error = %q", got)
	}
	if got := sanitizeTaskError(controlproto.TaskLogQuery, "failure secret-sentinel"); got != "node log query failed" {
		t.Fatalf("unknown error = %q", got)
	}
}
