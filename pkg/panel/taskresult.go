package panel

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
	"github.com/RSJWY/NativeS3-Bridge/pkg/logging"
)

const (
	maxRemoteLogFieldBytes    = 16 << 10
	maxRemoteLogAttrKeyBytes  = 256
	maxRemoteLogAttrsPerEntry = 64
	maxRemoteTaskErrorBytes   = 512
)

func sanitizeTaskResult(taskType controlproto.TaskType, result controlproto.TaskResult) controlproto.TaskResult {
	if taskType != controlproto.TaskLogQuery {
		return result
	}
	out := controlproto.TaskResult{LogSource: "ring", LogTruncated: result.LogTruncated}
	entryLimit := min(len(result.LogEntries), controlproto.MaxLogQueryLimit)
	if entryLimit < len(result.LogEntries) {
		out.LogTruncated = true
	}
	for _, entry := range result.LogEntries[:entryLimit] {
		clean, truncated := sanitizeRemoteLogEntry(entry)
		out.LogEntries = append(out.LogEntries, clean)
		out.LogTruncated = out.LogTruncated || truncated
	}
	lineLimit := min(len(result.LogLines), controlproto.MaxLogQueryLimit)
	if lineLimit < len(result.LogLines) {
		out.LogTruncated = true
	}
	for _, line := range result.LogLines[:lineLimit] {
		clean, truncated := truncateRemoteLogString(line, maxRemoteLogFieldBytes)
		out.LogLines = append(out.LogLines, clean)
		out.LogTruncated = out.LogTruncated || truncated
	}
	for {
		encoded, err := json.Marshal(out)
		if err == nil && len(encoded) <= controlproto.MaxLogQueryResultBytes {
			break
		}
		out.LogTruncated = true
		switch {
		case len(out.LogEntries) > 0 && len(out.LogLines) > 0:
			out.LogEntries = out.LogEntries[:len(out.LogEntries)-1]
			out.LogLines = out.LogLines[:len(out.LogLines)-1]
		case len(out.LogEntries) > 0:
			out.LogEntries = out.LogEntries[:len(out.LogEntries)-1]
		case len(out.LogLines) > 0:
			out.LogLines = out.LogLines[:len(out.LogLines)-1]
		default:
			return controlproto.TaskResult{LogSource: "ring", LogTruncated: true}
		}
	}
	return out
}

func sanitizeRemoteLogEntry(entry controlproto.TaskLogEntry) (controlproto.TaskLogEntry, bool) {
	clean := controlproto.TaskLogEntry{}
	var truncated bool
	var fieldTruncated bool
	clean.Time, fieldTruncated = truncateRemoteLogString(entry.Time, 64)
	truncated = truncated || fieldTruncated
	clean.Level, fieldTruncated = truncateRemoteLogString(strings.ToUpper(entry.Level), controlproto.MaxLogQueryLevelBytes)
	truncated = truncated || fieldTruncated
	clean.Msg, fieldTruncated = truncateRemoteLogString(entry.Msg, maxRemoteLogFieldBytes)
	truncated = truncated || fieldTruncated
	if len(entry.Attrs) == 0 {
		return clean, truncated
	}
	keys := make([]string, 0, len(entry.Attrs))
	for key := range entry.Attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	clean.Attrs = make(map[string]string)
	for _, key := range keys {
		if logging.IsSensitiveKey(key) {
			continue
		}
		if len(clean.Attrs) >= maxRemoteLogAttrsPerEntry {
			truncated = true
			break
		}
		cleanKey, keyTruncated := truncateRemoteLogString(key, maxRemoteLogAttrKeyBytes)
		cleanValue, valueTruncated := truncateRemoteLogString(entry.Attrs[key], maxRemoteLogFieldBytes)
		if cleanKey == "" {
			continue
		}
		clean.Attrs[cleanKey] = cleanValue
		truncated = truncated || keyTruncated || valueTruncated
	}
	if len(clean.Attrs) == 0 {
		clean.Attrs = nil
	}
	return clean, truncated
}

func truncateRemoteLogString(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end], true
}

func sanitizeTaskError(taskType controlproto.TaskType, message string) string {
	if taskType != controlproto.TaskLogQuery {
		clean, _ := truncateRemoteLogString(message, maxRemoteTaskErrorBytes)
		return clean
	}
	message = strings.TrimSpace(message)
	for _, allowed := range []string{
		"log ring is not configured",
		"log limit must not be negative",
		"log keyword is too long",
		"log level is too long",
		"log since must be RFC3339",
		"log until must be RFC3339",
		"log since must not be after until",
		"log query cancelled",
	} {
		if message == allowed {
			return message
		}
	}
	if message == "" {
		return ""
	}
	return "node log query failed"
}
