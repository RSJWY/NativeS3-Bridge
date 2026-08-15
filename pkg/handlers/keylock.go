package handlers

import (
	"hash/fnv"
	"sync"
)

// objectKeyLockShards 以固定分片串行化同一目的 key 的存储变更。遥测的对象数
// 增量依赖"变更前后对象是否存在":两个并发 PUT 同一 key 都可能观测到对象不
// 存在而各自 +1,磁盘上却只有一个对象。对同一 key,head -> 写入 -> 记账必须
// 在一个临界区内完成。分片而非逐 key 加锁是为了避免无界锁表;不同 key 偶尔
// 共享分片只损失一点并发度,不影响正确性。包级变量使 ObjectHandler 与
// MultipartHandler 对同一 key 互斥(单进程内有效,节点数据面只有本进程写盘)。
const objectKeyLockShards = 64

var objectKeyLocks sync.Map // shard index -> *sync.Mutex

// lockObjectKey 锁住 bucket/key 对应的分片,返回解锁函数。
func lockObjectKey(bucket, key string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucket))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	shard := h.Sum32() % objectKeyLockShards
	loaded, _ := objectKeyLocks.LoadOrStore(shard, &sync.Mutex{})
	mu := loaded.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// lockTelemetryObject acquires the node-wide telemetry mutation gate before
// the per-key lock. Every object-changing handler uses this order, while a
// full telemetry rebuild takes the exclusive side of the node-wide gate.
func lockTelemetryObject(recorder TelemetryRecorder, bucket, key string) func() {
	if recorder == nil {
		return func() {}
	}
	endMutation := recorder.BeginMutation()
	unlockKey := lockObjectKey(bucket, key)
	return func() {
		unlockKey()
		endMutation()
	}
}
