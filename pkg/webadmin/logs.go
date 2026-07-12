package webadmin

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/logging"
)

const lumberjackBackupTimeFormat = "2006-01-02T15-04-05.000"

type logFileInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Current    bool      `json:"current"`
	Compressed bool      `json:"compressed"`
	path       string
}

type logsResponse struct {
	Source       string          `json:"source"`
	FileEnabled  bool            `json:"file_enabled"`
	Limit        int             `json:"limit"`
	Entries      []logging.Entry `json:"entries"`
	Warning      string          `json:"warning,omitempty"`
	Files        []logFileInfo   `json:"files"`
	SelectedFile *logFileInfo    `json:"selected_file,omitempty"`
}

func (a *API) Logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := parseLogLimit(r.URL.Query().Get("limit"))
	level := strings.TrimSpace(r.URL.Query().Get("level"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	selectedID := r.URL.Query().Get("file")
	response := logsResponse{Source: "ring", FileEnabled: a.logFile != "", Limit: limit, Entries: []logging.Entry{}, Files: []logFileInfo{}}
	if a.logFile != "" {
		files, listErr := listLogFiles(a.logFile)
		if listErr == nil {
			response.Files = files
		}

		selected := currentLogFile(files)
		path := a.logFile
		compressed := false
		if selectedID != "" {
			if !validLogFileID(selectedID) {
				writeJSONError(w, http.StatusBadRequest, "invalid log file")
				return
			}
			if listErr != nil {
				writeJSONError(w, http.StatusInternalServerError, "list log files failed")
				return
			}
			selected = findLogFile(files, selectedID)
			if selected == nil {
				writeJSONError(w, http.StatusNotFound, "log file not found")
				return
			}
			path = selected.path
			compressed = selected.Compressed
		}
		response.SelectedFile = selected

		entries, err := tailLogFile(path, compressed, limit, level, query)
		if err == nil {
			response.Source = "file"
			response.Entries = entries
			if listErr != nil {
				response.Warning = "列出历史日志文件失败"
			}
			writeJSON(w, http.StatusOK, response)
			return
		}
		if selectedID != "" {
			if os.IsNotExist(err) {
				writeJSONError(w, http.StatusNotFound, "log file not found")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "read log file failed")
			return
		}
		response.Warning = "读取日志文件失败，已回退到内存日志"
	}
	if a.logRing != nil {
		response.Entries = a.logRing.Snapshot(limit, level, query)
	}
	writeJSON(w, http.StatusOK, response)
}

func listLogFiles(activeFile string) ([]logFileInfo, error) {
	directory := filepath.Dir(filepath.Clean(activeFile))
	activeName := filepath.Base(activeFile)
	canonicalDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	files := make([]logFileInfo, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		current := name == activeName
		if !current && !isLumberjackBackup(name, activeName) {
			continue
		}
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		canonicalPath, err := filepath.EvalSymlinks(path)
		if err != nil || !pathWithinDirectory(canonicalDirectory, canonicalPath) {
			continue
		}
		files = append(files, logFileInfo{
			ID:         name,
			Name:       name,
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
			Current:    current,
			Compressed: !current && strings.HasSuffix(name, ".gz"),
			path:       path,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Current != files[j].Current {
			return files[i].Current
		}
		if !files[i].ModifiedAt.Equal(files[j].ModifiedAt) {
			return files[i].ModifiedAt.After(files[j].ModifiedAt)
		}
		return files[i].Name > files[j].Name
	})
	return files, nil
}

func isLumberjackBackup(name, activeName string) bool {
	candidate := strings.TrimSuffix(name, ".gz")
	extension := filepath.Ext(activeName)
	stem := strings.TrimSuffix(activeName, extension)
	prefix := stem + "-"
	if !strings.HasPrefix(candidate, prefix) || !strings.HasSuffix(candidate, extension) {
		return false
	}
	timestamp := strings.TrimSuffix(strings.TrimPrefix(candidate, prefix), extension)
	parsed, err := time.Parse(lumberjackBackupTimeFormat, timestamp)
	return err == nil && parsed.Format(lumberjackBackupTimeFormat) == timestamp
}

func pathWithinDirectory(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validLogFileID(id string) bool {
	return id != "" && id != "." && id != ".." && !filepath.IsAbs(id) && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func currentLogFile(files []logFileInfo) *logFileInfo {
	for index := range files {
		if files[index].Current {
			file := files[index]
			return &file
		}
	}
	return nil
}

func findLogFile(files []logFileInfo, id string) *logFileInfo {
	for index := range files {
		if files[index].ID == id {
			file := files[index]
			return &file
		}
	}
	return nil
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

func tailLogFile(path string, compressed bool, limit int, level, query string) ([]logging.Entry, error) {
	reader, err := openLogReader(path, compressed)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	entries := make([]logging.Entry, 0, limit)
	scanner := bufio.NewScanner(reader)
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

func openLogReader(path string, compressed bool) (io.ReadCloser, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("log path is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) {
		file.Close()
		return nil, fmt.Errorf("log file changed during open")
	}
	if !compressed {
		return file, nil
	}
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	return &gzipLogReader{Reader: gzipReader, gzipReader: gzipReader, file: file}, nil
}

type gzipLogReader struct {
	io.Reader
	gzipReader *gzip.Reader
	file       *os.File
}

func (r *gzipLogReader) Close() error {
	gzipErr := r.gzipReader.Close()
	fileErr := r.file.Close()
	if gzipErr != nil {
		return gzipErr
	}
	return fileErr
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
