package handlers

// TelemetryRecorder 维护节点级存储使用计数(字节总量与对象数)。它在成功变更
// 原生存储后被调用,净增量由各 handler 依据"变更前后对象是否存在/大小"计算:
// 新建对象 +1,覆盖只记大小差,删除按是否存在 -1/0,零大小对象按存在计数。
// 失败的存储变更绝不调用它。实现方负责把失败记账显式标记为遥测不可用,
// 而不是伪造 0;standalone 构造路径保持 nil,行为不变。
type TelemetryRecorder interface {
	// BeginMutation keeps a storage mutation and its telemetry accounting in
	// one shared critical section. Rebuilds take the exclusive side of the same
	// gate, so a filesystem scan can never overlap native object changes.
	BeginMutation() func()
	RecordMutation(deltaBytes, deltaObjects int64)
}

// recordTelemetry 是 nil 安全的调用点封装。
func recordTelemetry(recorder TelemetryRecorder, deltaBytes, deltaObjects int64) {
	if recorder == nil {
		return
	}
	recorder.RecordMutation(deltaBytes, deltaObjects)
}

// telemetryPutObjectDelta 计算一次"落盘成功"写操作的对象数增量:覆盖已存在
// 对象不变,新建(含零大小)对象 +1。
func telemetryPutObjectDelta(replaced bool) int64 {
	if replaced {
		return 0
	}
	return 1
}

// telemetryDeleteObjectDelta 计算一次成功删除的对象数增量:删除了已存在对象
// (含零大小)才 -1,目标不存在不动。
func telemetryDeleteObjectDelta(deleted bool) int64 {
	if deleted {
		return -1
	}
	return 0
}
