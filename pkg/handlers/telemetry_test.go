package handlers

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/auth"
	"github.com/RSJWY/NativeS3-Bridge/pkg/storage"
)

// telemetryRecorderFake 记录每次成功变更的净增量,验证 handlers 层的换算。
type telemetryRecorderFake struct {
	mutations [][2]int64
}

func (f *telemetryRecorderFake) BeginMutation() func() { return func() {} }

func (f *telemetryRecorderFake) RecordMutation(deltaBytes, deltaObjects int64) {
	f.mutations = append(f.mutations, [2]int64{deltaBytes, deltaObjects})
}

func (f *telemetryRecorderFake) total() (bytes int64, objects int64) {
	for _, m := range f.mutations {
		bytes += m[0]
		objects += m[1]
	}
	return bytes, objects
}

func newTelemetryTestHandler(t *testing.T) (*ObjectHandler, *telemetryRecorderFake, storage.Backend) {
	t.Helper()
	backend, err := storage.NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	recorder := &telemetryRecorderFake{}
	h := NewObjectHandler(backend, nil)
	h.SetTelemetryRecorder(recorder)
	return h, recorder, backend
}

func authedPut(key string, body string) (*http.Request, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPut, "/test-bucket/"+key, strings.NewReader(body))
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 1, AccessKey: "ak"}))
	return req, httptest.NewRecorder()
}

// PUT 新建/覆盖矩阵:新建按大小 +1,覆盖只记大小差,零大小对象也 +1。
func TestTelemetryPutNewOverwriteAndZeroByte(t *testing.T) {
	h, recorder, _ := newTelemetryTestHandler(t)

	req, rr := authedPut("a.txt", "12345")
	h.Put(rr, req, "test-bucket", "a.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("put a.txt = %d", rr.Code)
	}
	// 覆盖:10 字节替换 5 字节。
	req, rr = authedPut("a.txt", "0123456789")
	h.Put(rr, req, "test-bucket", "a.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("overwrite a.txt = %d", rr.Code)
	}
	// 零大小新对象。
	req, rr = authedPut("empty.txt", "")
	h.Put(rr, req, "test-bucket", "empty.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("put empty = %d", rr.Code)
	}

	want := [][2]int64{{5, 1}, {5, 0}, {0, 1}}
	if len(recorder.mutations) != len(want) {
		t.Fatalf("mutations = %+v, want %+v", recorder.mutations, want)
	}
	for i, m := range want {
		if recorder.mutations[i] != m {
			t.Fatalf("mutation %d = %v, want %v", i, recorder.mutations[i], m)
		}
	}
}

// 失败的 PUT(存储未落盘)不得改变计数。
func TestTelemetryFailedPutNotRecorded(t *testing.T) {
	h, recorder, _ := newTelemetryTestHandler(t)
	body := "12345"
	sum := md5.Sum([]byte(body))
	badSum := md5.Sum([]byte("other"))

	req := httptest.NewRequest(http.MethodPut, "/test-bucket/bad.txt", strings.NewReader(body))
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(badSum[:]))
	req.Header.Set("X-Amz-Decoded-Content-Length", body)
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 1, AccessKey: "ak"}))
	rr := httptest.NewRecorder()
	h.Put(rr, req, "test-bucket", "bad.txt")
	if rr.Code == http.StatusOK {
		t.Fatalf("corrupt md5 put should fail, got %d", rr.Code)
	}
	_ = sum
	if len(recorder.mutations) != 0 {
		t.Fatalf("failed put mutated telemetry: %+v", recorder.mutations)
	}
}

// 删除矩阵:已存在对象 -1(含零大小),目标不存在不动。
func TestTelemetryDeleteExistingMissingAndZeroByte(t *testing.T) {
	h, recorder, _ := newTelemetryTestHandler(t)
	for key, body := range map[string]string{"a.txt": "12345", "z.txt": ""} {
		req, rr := authedPut(key, body)
		if _, err := h.backend.PutObject("test-bucket", key, strings.NewReader(body), "text/plain"); err != nil {
			t.Fatal(err)
		}
		_ = req
		_ = rr
	}
	recorder.mutations = nil

	doDelete := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/test-bucket/"+key, nil)
		rr := httptest.NewRecorder()
		h.Delete(rr, req, "test-bucket", key)
		return rr
	}
	if rr := doDelete("a.txt"); rr.Code != http.StatusNoContent {
		t.Fatalf("delete a.txt = %d", rr.Code)
	}
	if rr := doDelete("missing.txt"); rr.Code != http.StatusNoContent {
		t.Fatalf("delete missing = %d", rr.Code)
	}
	if rr := doDelete("z.txt"); rr.Code != http.StatusNoContent {
		t.Fatalf("delete z.txt = %d", rr.Code)
	}

	want := [][2]int64{{-5, -1}, {0, 0}, {0, -1}}
	if len(recorder.mutations) != len(want) {
		t.Fatalf("mutations = %+v, want %+v", recorder.mutations, want)
	}
	for i, m := range want {
		if recorder.mutations[i] != m {
			t.Fatalf("mutation %d = %v, want %v", i, recorder.mutations[i], m)
		}
	}
}

// 服务端 Copy 矩阵:复制到新 key 记 +size/+1,覆盖记大小差。
func TestTelemetryCopyNewKeyAndOverwrite(t *testing.T) {
	h, recorder, backend := newTelemetryTestHandler(t)
	if _, err := backend.PutObject("test-bucket", "src.txt", strings.NewReader("12345"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PutObject("test-bucket", "small.txt", strings.NewReader("ab"), "text/plain"); err != nil {
		t.Fatal(err)
	}

	doCopy := func(dst string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/test-bucket/"+dst, nil)
		req.Header.Set("x-amz-copy-source", "/test-bucket/src.txt")
		req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 1, AccessKey: "ak"}))
		rr := httptest.NewRecorder()
		h.Copy(rr, req, "test-bucket", dst)
		return rr
	}
	if rr := doCopy("dst.txt"); rr.Code != http.StatusOK {
		t.Fatalf("copy new = %d %s", rr.Code, rr.Body.String())
	}
	if rr := doCopy("small.txt"); rr.Code != http.StatusOK {
		t.Fatalf("copy overwrite = %d %s", rr.Code, rr.Body.String())
	}

	want := [][2]int64{{5, 1}, {3, 0}}
	if len(recorder.mutations) != len(want) {
		t.Fatalf("mutations = %+v, want %+v", recorder.mutations, want)
	}
	for i, m := range want {
		if recorder.mutations[i] != m {
			t.Fatalf("mutation %d = %v, want %v", i, recorder.mutations[i], m)
		}
	}
}

// 批量删除:每个真实删除的对象单独扣减,缺失对象不动。
func TestTelemetryBatchDeleteMixesExistingAndMissing(t *testing.T) {
	h, recorder, backend := newTelemetryTestHandler(t)
	if _, err := backend.PutObject("test-bucket", "a.txt", strings.NewReader("1234"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PutObject("test-bucket", "z.txt", strings.NewReader(""), "text/plain"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/test-bucket?delete",
		strings.NewReader(`<Delete><Object><Key>a.txt</Key></Object><Object><Key>z.txt</Key></Object><Object><Key>missing.txt</Key></Object></Delete>`))
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 1, AccessKey: "ak"}))
	rr := httptest.NewRecorder()
	h.DeleteObjects(rr, req, "test-bucket")
	if rr.Code != http.StatusOK {
		t.Fatalf("batch delete = %d", rr.Code)
	}
	gotBytes, gotObjects := recorder.total()
	if gotBytes != -4 || gotObjects != -2 {
		t.Fatalf("batch delete deltas = %d bytes / %d objects, want -4 / -2", gotBytes, gotObjects)
	}
}

// 并发 PUT 同一 key:磁盘上最终只有一个对象,对象数增量合计必须恰好 +1。
// 回归防护:分片锁让 head -> 写入 -> 记账对同一 key 原子。
func TestTelemetryConcurrentPutSameKeyCountsOnce(t *testing.T) {
	h, recorder, backend := newTelemetryTestHandler(t)
	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, rr := authedPut("race.txt", "body")
			h.Put(rr, req, "test-bucket", "race.txt")
			if rr.Code != http.StatusOK {
				t.Errorf("concurrent put = %d", rr.Code)
			}
		}()
	}
	wg.Wait()

	_, objects := recorder.total()
	if objects != 1 {
		t.Fatalf("same-key concurrent put object delta = %d, want 1", objects)
	}
	listing, err := backend.ListObjects("test-bucket", "", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Objects) != 1 {
		t.Fatalf("disk objects = %d, want 1", len(listing.Objects))
	}
}

// Multipart Complete:新建 +total/+1;覆盖既有对象记大小差、对象数不变。
func TestTelemetryMultipartCompleteNewAndOverwrite(t *testing.T) {
	dataRoot := t.TempDir()
	backend, err := storage.NewFileBackendWithMetadataSuffix(dataRoot, storage.DefaultMetadataSuffix)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewMultipartStore(dataRoot, filepath.Join(dataRoot, ".multipart"), storage.DefaultMetadataSuffix)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &telemetryRecorderFake{}
	h := NewMultipartHandlerWithHooks(store, nil, nil)
	h.SetTelemetryRecorder(recorder)

	upload := func(bucket, key, part1 string) {
		t.Helper()
		uploadID, err := store.Create(bucket, key, "text/plain", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		etag, err := store.UploadPart(uploadID, 1, strings.NewReader(part1))
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/"+bucket+"/"+key+"?uploadId="+uploadID+"&partNumber=1",
			strings.NewReader(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>`+etag+`</ETag></Part></CompleteMultipartUpload>`))
		req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{CredentialID: 1, AccessKey: "ak"}))
		rr := httptest.NewRecorder()
		h.Complete(rr, req, bucket, key)
		if rr.Code != http.StatusOK {
			t.Fatalf("complete %s/%s = %d %s", bucket, key, rr.Code, rr.Body.String())
		}
	}

	upload("test-bucket", "mp.txt", "12345")
	// 覆盖为更长的内容。
	upload("test-bucket", "mp.txt", "1234567890")

	want := [][2]int64{{5, 1}, {5, 0}}
	if len(recorder.mutations) != len(want) {
		t.Fatalf("mutations = %+v, want %+v", recorder.mutations, want)
	}
	for i, m := range want {
		if recorder.mutations[i] != m {
			t.Fatalf("mutation %d = %v, want %v", i, recorder.mutations[i], m)
		}
	}
	_ = backend
	_ = bytes.MinRead
}
