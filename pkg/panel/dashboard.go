package panel

import (
	"net/http"
	"sort"
	"time"
)

// Panel 首页仪表盘的只读汇总。所有字段都来自数据库状态和 Hub 实时连接观测,
// 不推算容量、对象数或请求量(那些需要节点采集协议,见任务 Deferred Metrics)。
// 聚合复用 nodeToResponse,保证仪表盘与节点列表的状态口径一致。

type dashboardTotals struct {
	Nodes     int `json:"nodes"`
	Online    int `json:"online"`
	Offline   int `json:"offline"`
	Retired   int `json:"retired"`
	Attention int `json:"attention"`
}

type dashboardHealth struct {
	Synced  int `json:"synced"`
	Waiting int `json:"waiting"`
	Failed  int `json:"failed"`
	Drift   int `json:"drift"`
	Unknown int `json:"unknown"`
}

// dashboardAttentionNode 是需要处理的节点摘要。字段与 nodeResponse 同源,
// 额外的 severity 是后端派生的展示排序键,不让前端复制业务判断。
type dashboardAttentionNode struct {
	ID              uint       `json:"id"`
	DisplayName     string     `json:"display_name"`
	Status          string     `json:"status"`
	Online          bool       `json:"online"`
	AppliedVersion  int64      `json:"applied_version"`
	DesiredVersion  int64      `json:"desired_version"`
	SyncState       string     `json:"sync_state"`
	LastError       string     `json:"last_error,omitempty"`
	DraftDirty      bool       `json:"draft_dirty"`
	PublishRequired bool       `json:"publish_required"`
	Region          string     `json:"region,omitempty"`
	LastHeartbeat   *time.Time `json:"last_heartbeat,omitempty"`
	Severity        string     `json:"severity"`
}

type dashboardSummaryResponse struct {
	Totals         dashboardTotals          `json:"totals"`
	Health         dashboardHealth          `json:"health"`
	Telemetry      dashboardTelemetry       `json:"telemetry"`
	AttentionNodes []dashboardAttentionNode `json:"attention_nodes"`
	GeneratedAt    time.Time                `json:"generated_at"`
}

// 节点遥测状态:有效 / 未上报(旧 agent 或字段不完整) / 已过期。
const (
	telemetryStatusValid   = "valid"
	telemetryStatusMissing = "missing"
	telemetryStatusStale   = "stale"
)

type dashboardTelemetry struct {
	UsedBytesTotal int64                    `json:"used_bytes_total"`
	ObjectCount    int64                    `json:"object_count"`
	ValidNodes     int                      `json:"valid_nodes"`
	MissingNodes   int                      `json:"missing_nodes"`
	StaleNodes     int                      `json:"stale_nodes"`
	Nodes          []dashboardNodeTelemetry `json:"nodes"`
}

// dashboardNodeTelemetry 是单个节点的最新遥测摘要。值为 null 表示该节点
// 未上报完整遥测,前端必须显示"未上报/不可用",绝不能显示 0。
type dashboardNodeTelemetry struct {
	NodeID      uint       `json:"node_id"`
	DisplayName string     `json:"display_name"`
	UsedBytes   *int64     `json:"used_bytes"`
	ObjectCount *int64     `json:"object_count"`
	ObservedAt  *time.Time `json:"observed_at"`
	Status      string     `json:"status"`
}

// Severity 顺序:同步失败/漂移 > 离线 > 待同步/待发布。
const (
	severitySyncFailed = "sync_failed"
	severityDrift      = "drift"
	severityOffline    = "offline"
	severityPending    = "pending"
)

var severityRank = map[string]int{
	severitySyncFailed: 4,
	severityDrift:      3,
	severityOffline:    2,
	severityPending:    1,
}

// DashboardSummary 聚合节点生命周期、连接状态、同步健康和需要关注的节点,
// 一次响应覆盖前端首屏,避免对每个节点发起 N+1 请求。
func (a *AdminAPI) DashboardSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var nodes []Node
	if err := a.db.Order("id ASC").Find(&nodes).Error; err != nil {
		writeTransportError(w, http.StatusInternalServerError, "query nodes failed")
		return
	}
	// 一次性载入所有 NodeState,避免逐节点查询;遥测与心跳口径共用这一份行。
	states := make(map[uint]*NodeState, len(nodes))
	if len(nodes) > 0 {
		var nodeStates []NodeState
		if err := a.db.Find(&nodeStates).Error; err != nil {
			writeTransportError(w, http.StatusInternalServerError, "query node states failed")
			return
		}
		for i := range nodeStates {
			states[nodeStates[i].NodeID] = &nodeStates[i]
		}
	}
	generated := nowUTC()
	telemetryExpiry := a.effectiveTelemetryExpiry()
	resp := dashboardSummaryResponse{
		AttentionNodes: make([]dashboardAttentionNode, 0, len(nodes)),
		Telemetry:      dashboardTelemetry{Nodes: make([]dashboardNodeTelemetry, 0, len(nodes))},
		GeneratedAt:    generated,
	}
	for _, n := range nodes {
		resp.Totals.Nodes++
		if n.Status == NodeStatusRetired {
			// 退役节点单独计数:不计入在线/离线或健康分布,即使 Hub 残留
			// 在线连接也不能被误判为健康在线节点。
			resp.Totals.Retired++
			continue
		}
		state := a.nodeToResponse(n)
		if state.Online {
			resp.Totals.Online++
		} else {
			resp.Totals.Offline++
		}
		switch state.SyncState {
		case SyncStateSynced:
			resp.Health.Synced++
		case SyncStateWaiting:
			resp.Health.Waiting++
		case SyncStateFailed:
			resp.Health.Failed++
		case SyncStateDrift:
			resp.Health.Drift++
		default:
			// 没有 NodeState 或未上报同步状态的节点进入 unknown。
			resp.Health.Unknown++
		}
		// 需要处理的口径与 design §3 统一:同步失败/漂移 > 非退役离线 >
		// waiting/draft_dirty/publish_required。即除"在线且已同步、无待发布
		// 标记"的健康节点外,其余非退役节点都进入关注列表。
		if attention := state.SyncState == SyncStateFailed || state.SyncState == SyncStateDrift ||
			!state.Online || state.SyncState == SyncStateWaiting ||
			state.DraftDirty || state.PublishRequired; attention {
			resp.Totals.Attention++
			resp.AttentionNodes = append(resp.AttentionNodes, dashboardAttentionNode{
				ID: state.ID, DisplayName: state.DisplayName, Status: state.Status,
				Online: state.Online, AppliedVersion: state.AppliedVersion, DesiredVersion: state.DesiredVersion,
				SyncState: state.SyncState, LastError: state.LastError, DraftDirty: state.DraftDirty,
				PublishRequired: state.PublishRequired, Region: state.Region,
				LastHeartbeat: state.LastHeartbeat, Severity: attentionSeverity(state),
			})
		}
		// 节点遥测摘要:缺失/过期节点不贡献总量,也不被记成 0。
		entry, contributes := nodeTelemetrySummary(n, states[n.ID], generated, telemetryExpiry)
		switch {
		case contributes:
			resp.Telemetry.UsedBytesTotal += *entry.UsedBytes
			resp.Telemetry.ObjectCount += *entry.ObjectCount
			resp.Telemetry.ValidNodes++
		case entry.Status == telemetryStatusStale:
			resp.Telemetry.StaleNodes++
		default:
			resp.Telemetry.MissingNodes++
		}
		resp.Telemetry.Nodes = append(resp.Telemetry.Nodes, entry)
	}
	sortAttentionNodes(resp.AttentionNodes)
	writeTransportJSON(w, http.StatusOK, resp)
}

// nodeTelemetrySummary 把单个节点的 NodeState 遥测列归类为 valid/missing/stale。
// contributes=true 表示这是"有效且未过期"的观测,应当计入总量;UsedBytes/
// ObjectCount 为 nil 时表示未上报,由前端显示"未上报/不可用"。
func nodeTelemetrySummary(n Node, state *NodeState, generated time.Time, expiry time.Duration) (dashboardNodeTelemetry, bool) {
	entry := dashboardNodeTelemetry{NodeID: n.ID, DisplayName: n.DisplayName, Status: telemetryStatusMissing}
	if state == nil {
		return entry, false
	}
	entry.UsedBytes = state.UsedBytesTotal
	entry.ObjectCount = state.ObjectCount
	entry.ObservedAt = state.TelemetryObservedAt
	// 当前不可用(旧版心跳覆盖过)或从未上报过完整快照:一律 missing。
	if !state.TelemetryValid || state.UsedBytesTotal == nil || state.ObjectCount == nil ||
		state.TelemetryObservedAt == nil || state.TelemetryObservedAt.IsZero() {
		return entry, false
	}
	// 过期判断与离线判定同口径:观测时间超出阈值即不再代表当前状态。
	if state.TelemetryObservedAt.Before(generated.Add(-expiry)) {
		entry.Status = telemetryStatusStale
		return entry, false
	}
	entry.Status = telemetryStatusValid
	return entry, true
}

// attentionSeverity 只对已进入需要关注集合的节点分级:同步失败/漂移最高,
// 其次非退役离线(含离线且 waiting 的节点),最后是待同步/待发布。
func attentionSeverity(state nodeResponse) string {
	switch {
	case state.SyncState == SyncStateFailed:
		return severitySyncFailed
	case state.SyncState == SyncStateDrift:
		return severityDrift
	case !state.Online:
		return severityOffline
	default:
		return severityPending
	}
}

// sortAttentionNodes 固定排序:严重性降序;同级按最近心跳从旧到新,缺失心跳
// 排最后;最后按节点 ID 升序保证稳定结果,不依赖数据库返回顺序。
func sortAttentionNodes(items []dashboardAttentionNode) {
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := severityRank[items[i].Severity], severityRank[items[j].Severity]
		if ri != rj {
			return ri > rj
		}
		switch {
		case items[i].LastHeartbeat == nil && items[j].LastHeartbeat == nil:
		case items[i].LastHeartbeat == nil:
			return false
		case items[j].LastHeartbeat == nil:
			return true
		default:
			if !items[i].LastHeartbeat.Equal(*items[j].LastHeartbeat) {
				return items[i].LastHeartbeat.Before(*items[j].LastHeartbeat)
			}
		}
		return items[i].ID < items[j].ID
	})
}
