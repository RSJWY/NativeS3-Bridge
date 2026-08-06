package nodeagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/logging"
)

func TestLogQueryReturnsStructuredFilteredBoundedResult(t *testing.T) {
	ring := logging.NewRing(10)
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	ring.Append(logging.Entry{Time: base, Level: "INFO", Message: "old"})
	ring.Append(logging.Entry{Time: base.Add(time.Minute), Level: "ERROR", Message: "media failed", Attrs: map[string]any{
		"node": 7, "secret_token": "must-not-leak",
	}})
	ring.Append(logging.Entry{Time: base.Add(2 * time.Minute), Level: "ERROR", Message: "other"})
	runner := NewLocalTaskRunner(nil, ring, "", "", nil)

	result := runner.Run(context.Background(), controlproto.TaskPayload{
		TaskID: "log-1", Type: controlproto.TaskLogQuery,
		Params: controlproto.TaskParams{
			Since: base.Add(time.Minute).Format(time.RFC3339Nano), Until: base.Add(2 * time.Minute).Format(time.RFC3339Nano),
			Level: "error", Keyword: "MEDIA", Limit: 10,
		},
	})
	if result.State != controlproto.TaskStateSuccess {
		t.Fatalf("state = %s error=%q", result.State, result.Error)
	}
	if result.Result.LogSource != "ring" || len(result.Result.LogEntries) != 1 || len(result.Result.LogLines) != 1 {
		t.Fatalf("result = %+v", result.Result)
	}
	entry := result.Result.LogEntries[0]
	if entry.Msg != "media failed" || entry.Level != "ERROR" || entry.Attrs["node"] != "7" {
		t.Fatalf("entry = %+v", entry)
	}
	if _, exists := entry.Attrs["secret_token"]; exists || strings.Contains(result.Result.LogLines[0], "must-not-leak") {
		t.Fatalf("result leaked sensitive attr: %+v", result.Result)
	}
}

func TestLogQueryRejectsInvalidParamsWithoutEchoingInput(t *testing.T) {
	ring := logging.NewRing(1)
	runner := NewLocalTaskRunner(nil, ring, "", "", nil)
	secret := "secret-sentinel"
	for _, params := range []controlproto.TaskParams{
		{Limit: -1},
		{Since: secret},
		{Since: "2026-07-25T11:00:00Z", Until: "2026-07-25T10:00:00Z"},
		{Keyword: strings.Repeat("x", controlproto.MaxLogQueryKeywordBytes+1)},
	} {
		result := runner.Run(context.Background(), controlproto.TaskPayload{TaskID: "bad", Type: controlproto.TaskLogQuery, Params: params})
		if result.State != controlproto.TaskStateFailed || result.Error == "" {
			t.Fatalf("result = %+v", result)
		}
		if strings.Contains(result.Error, secret) {
			t.Fatalf("error echoed input: %q", result.Error)
		}
	}
}

func TestLogQueryEnforcesCountAndSerializedByteCeilings(t *testing.T) {
	ring := logging.NewRing(700)
	for index := 0; index < 600; index++ {
		ring.Append(logging.Entry{Time: time.Unix(int64(index), 0), Level: "INFO", Message: strings.Repeat("x", 1024), Attrs: map[string]any{"index": index}})
	}
	runner := NewLocalTaskRunner(nil, ring, "", "", nil)
	result := runner.Run(context.Background(), controlproto.TaskPayload{
		TaskID: "bounded", Type: controlproto.TaskLogQuery, Params: controlproto.TaskParams{Limit: 999},
	})
	if result.State != controlproto.TaskStateSuccess || !result.Result.LogTruncated {
		t.Fatalf("result state/truncation = %+v", result)
	}
	if len(result.Result.LogEntries) > controlproto.MaxLogQueryLimit || len(result.Result.LogLines) != len(result.Result.LogEntries) {
		t.Fatalf("entry counts = structured %d legacy %d", len(result.Result.LogEntries), len(result.Result.LogLines))
	}
	encoded, err := json.Marshal(result.Result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > controlproto.MaxLogQueryResultBytes {
		t.Fatalf("encoded result = %d bytes", len(encoded))
	}
}

func TestLogQueryHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := NewLocalTaskRunner(nil, logging.NewRing(1), "", "", nil)
	result := runner.Run(ctx, controlproto.TaskPayload{TaskID: "cancelled", Type: controlproto.TaskLogQuery})
	if result.State != controlproto.TaskStateFailed || result.Error != "log query cancelled" {
		t.Fatalf("result = %+v", result)
	}
}
