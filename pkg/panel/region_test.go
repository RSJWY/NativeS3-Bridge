package panel

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RSJWY/NativeS3-Bridge/pkg/db"
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

// 升级路径:已有 node_states 数据的旧库(无 region 列)执行 Migrate 必须加列成功,
// 且原有观测数据一行不丢。新建库天然带 region 列,覆盖不到这条路径。
func TestMigrateAddsRegionColumnToExistingNodeStates(t *testing.T) {
	gdb, err := db.Open("sqlite", filepath.Join(t.TempDir(), "panel-legacy.db"))
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("get sqlite handle: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close sqlite db: %v", err)
		}
	})

	// 本次改动之前的 node_states 形状,逐字段照抄旧 NodeState 定义。
	legacySchema := `CREATE TABLE node_states (
		id integer PRIMARY KEY AUTOINCREMENT,
		node_id integer NOT NULL,
		online numeric NOT NULL DEFAULT false,
		applied_version integer NOT NULL DEFAULT 0,
		sync_state text NOT NULL DEFAULT "waiting",
		content_hash text,
		last_error text,
		last_heartbeat datetime,
		updated_at datetime
	)`
	if err := gdb.Exec(legacySchema).Error; err != nil {
		t.Fatalf("create legacy node_states: %v", err)
	}
	if err := gdb.Exec(`CREATE UNIQUE INDEX idx_node_states_node_id ON node_states(node_id)`).Error; err != nil {
		t.Fatalf("create legacy index: %v", err)
	}
	if err := gdb.Exec(
		`INSERT INTO node_states (node_id, online, applied_version, sync_state, content_hash) VALUES (?, ?, ?, ?, ?)`,
		9, true, 42, SyncStateSynced, "legacy-hash",
	).Error; err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if gdb.Migrator().HasColumn(&NodeState{}, "region") {
		t.Fatal("legacy fixture already has a region column; it no longer models the pre-change schema")
	}

	if err := Migrate(gdb); err != nil {
		t.Fatalf("migrate legacy panel db: %v", err)
	}

	if !gdb.Migrator().HasColumn(&NodeState{}, "region") {
		t.Fatal("migrate did not add the region column to an existing node_states table")
	}
	var st NodeState
	if err := gdb.Where("node_id = ?", 9).First(&st).Error; err != nil {
		t.Fatalf("load pre-existing row after migrate: %v", err)
	}
	if st.AppliedVersion != 42 || st.ContentHash != "legacy-hash" || st.SyncState != SyncStateSynced || !st.Online {
		t.Fatalf("migrate lost pre-existing observed state: %+v", st)
	}
	if st.Region != "" {
		t.Fatalf("backfilled region = %q, want empty (unknown until the node reports)", st.Region)
	}
}
