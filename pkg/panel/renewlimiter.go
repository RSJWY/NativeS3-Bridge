package panel

import (
	"sync"
	"time"
)

// renewLimiter 按节点身份限制 /renew 的调用频率。每次续期都要 CA 签名并往
// node_certs 插一行,不限频等于给了一个免费的 CPU + 表膨胀入口:一个拿到合法客户端
// 证书的节点(或复制了该证书的攻击者)可以无限循环续期。
//
// 计数放内存即可:panel 是单进程,重启清零可以接受——窗口只有 1 小时,而正常续期是
// 每 90 天一次,重启后放过几次的代价远小于引入一张限频表的复杂度。
type renewLimiter struct {
	mu     sync.Mutex
	events map[uint][]time.Time
}

func newRenewLimiter() *renewLimiter {
	return &renewLimiter{events: make(map[uint][]time.Time)}
}

// allow 记录一次 nodeID 的续期尝试。窗口内次数未超限时返回 (0, true);超限时返回
// 距最早一次尝试滚出窗口所需的时间与 false,供调用方写 Retry-After。
//
// 超限的尝试不入账,否则持续重试会不断把窗口往后推,变成"越试越久"的惩罚性锁定,
// 而这里要的只是限流。
func (l *renewLimiter) allow(nodeID uint, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-renewWindow)
	kept := l.events[nodeID][:0]
	for _, ts := range l.events[nodeID] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}

	if len(kept) >= maxRenewPerWindow {
		l.events[nodeID] = kept
		// kept[0] 是窗口内最早的一次尝试,它滚出窗口后就又有额度了。
		retryAfter := kept[0].Sub(cutoff)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return retryAfter, false
	}

	l.events[nodeID] = append(kept, now)
	return 0, true
}
