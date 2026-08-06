package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSanitizeReportedRegion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "trims surrounding space", in: "  us-east-1\t", want: "us-east-1"},
		{name: "strips control characters", in: "us\x00-east\x1b-1", want: "us-east-1"},
		{name: "empty stays empty", in: "   ", want: ""},
		{name: "keeps a normal region unchanged", in: "cn-north-1", want: "cn-north-1"},
		{
			name: "truncates to the column width",
			in:   strings.Repeat("r", maxReportedRegionBytes+10),
			want: strings.Repeat("r", maxReportedRegionBytes),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeReportedRegion(tc.in); got != tc.want {
				t.Fatalf("sanitizeReportedRegion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// 区域是"每次 hello 的观测值",不是累积状态:节点降级到不上报 region 的旧 agent
// 后必须回到未上报,否则页面会拿上一次连接的旧值冒充当前运行区域。
func TestRecordHelloObservationOverwritesRegionWithEachHello(t *testing.T) {
	gdb := openTestDB(t)
	srv := NewTransportServer(TransportDeps{DB: gdb})

	srv.recordHelloObservation(1, 3, "hash-a", " ap-southeast-2 ")
	var st NodeState
	if err := gdb.Where("node_id = ?", 1).First(&st).Error; err != nil {
		t.Fatalf("load node state after first hello: %v", err)
	}
	if st.Region != "ap-southeast-2" {
		t.Fatalf("region after first hello = %q, want ap-southeast-2", st.Region)
	}

	// 同一节点用不上报 region 的旧 agent 重连。
	srv.recordHelloObservation(1, 3, "hash-a", "")
	if err := gdb.Where("node_id = ?", 1).First(&st).Error; err != nil {
		t.Fatalf("load node state after downgrade hello: %v", err)
	}
	if st.Region != "" {
		t.Fatalf("region after downgrade hello = %q, want empty (stale value must not survive)", st.Region)
	}
	if st.AppliedVersion != 3 || st.ContentHash != "hash-a" {
		t.Fatalf("region write disturbed applied state: %+v", st)
	}
}

func TestNodeResponseExposesReportedRegion(t *testing.T) {
	api, _ := newTestAdminAPI(t)
	if rw := serve(api, http.MethodPost, "/api/admin/nodes", `{"display_name":"node-a"}`); rw.Code != http.StatusCreated {
		t.Fatalf("create node status = %d, body=%s", rw.Code, rw.Body.String())
	}

	// 尚未连接过的节点没有 node_states 行,region 必须缺省而不是报错。
	rw := serve(api, http.MethodGet, "/api/admin/nodes/1", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("get node status = %d, body=%s", rw.Code, rw.Body.String())
	}
	var resp nodeResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode node: %v", err)
	}
	if resp.Region != "" {
		t.Fatalf("region before any hello = %q, want empty", resp.Region)
	}
	if strings.Contains(rw.Body.String(), `"region"`) {
		t.Fatalf("unreported region must be omitted from the body, got %s", rw.Body.String())
	}

	if err := api.db.Create(&NodeState{NodeID: 1, SyncState: SyncStateWaiting, Region: "cn-north-1"}).Error; err != nil {
		t.Fatalf("seed node state: %v", err)
	}
	rw = serve(api, http.MethodGet, "/api/admin/nodes/1", "")
	if rw.Code != http.StatusOK {
		t.Fatalf("get node status = %d, body=%s", rw.Code, rw.Body.String())
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode node: %v", err)
	}
	if resp.Region != "cn-north-1" {
		t.Fatalf("region = %q, want cn-north-1", resp.Region)
	}
}
