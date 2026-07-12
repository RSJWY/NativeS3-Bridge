package webadmin

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"net/http"

	"github.com/RSJWY/NativeS3-Bridge/pkg/logging"
)

type logsResponse struct {
	Source      string          `json:"source"`
	FileEnabled bool            `json:"file_enabled"`
	Limit       int             `json:"limit"`
	Entries     []logging.Entry `json:"entries"`
	Warning     string          `json:"warning,omitempty"`
}

func (a *API) Logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parseLogLimit(r.URL.Query().Get("limit"))
	level := strings.TrimSpace(r.URL.Query().Get("level"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	response := logsResponse{Source: "ring", FileEnabled: a.logFile != "", Limit: limit, Entries: []logging.Entry{}}
	if a.logFile != "" {
		entries, err := tailLogFile(a.logFile, limit, level, query)
		if err == nil {
			response.Source = "file"
			response.Entries = entries
			writeJSON(w, http.StatusOK, response)
			return
		}
		response.Warning = "读取日志文件失败，已回退到内存日志"
	}
	if a.logRing != nil {
		response.Entries = a.logRing.Snapshot(limit, level, query)
	}
	writeJSON(w, http.StatusOK, response)
}

func parseLogLimit(value string) int {
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 {
		return 200
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func tailLogFile(path string, limit int, level, query string) ([]logging.Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries := make([]logging.Entry, 0, limit)
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		entry := parseTextLogLine(scanner.Text())
		if level != "" && !strings.EqualFold(entry.Level, level) {
			continue
		}
		if query != "" && !logEntryMatches(entry, query) {
			continue
		}
		entries = append(entries, entry)
		if len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return entries, nil
}

func parseTextLogLine(line string) logging.Entry {
	entry := logging.Entry{Level: "INFO", Message: line, Attrs: map[string]any{}}
	fields := splitLogFields(line)
	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], strings.Trim(parts[1], `"`)
		switch key {
		case "time":
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				entry.Time = parsed.UTC()
			}
		case "level":
			entry.Level = value
		case "msg":
			entry.Message = value
		default:
			if !sensitiveLogKey(key) {
				entry.Attrs[key] = value
			}
		}
	}
	return entry
}

func splitLogFields(line string) []string {
	var fields []string
	var current strings.Builder
	quoted := false
	escaped := false
	for _, char := range line {
		if escaped {
			current.WriteRune(char)
			escaped = false
			continue
		}
		if char == '\\' && quoted {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
			current.WriteRune(char)
			continue
		}
		if char == ' ' && !quoted {
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(char)
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}

func logEntryMatches(entry logging.Entry, query string) bool {
	query = strings.ToLower(query)
	if strings.Contains(strings.ToLower(entry.Message), query) {
		return true
	}
	for key, value := range entry.Attrs {
		if strings.Contains(strings.ToLower(key), query) || strings.Contains(strings.ToLower(fmt.Sprint(value)), query) {
			return true
		}
	}
	return false
}

func sensitiveLogKey(key string) bool {
	key = strings.ToLower(key)
	for _, part := range []string{"secret", "password", "authorization", "cookie", "signature", "token"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}
