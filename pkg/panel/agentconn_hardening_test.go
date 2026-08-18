package panel

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/RSJWY/NativeS3-Bridge/pkg/controlproto"
)

// R3(AC2):写路径的等待必须可中断。旧实现用 sync.Mutex 串行化写,锁等待不接受
// ctx——一个"不读"的节点会让后续写方永久卡在 Lock() 上,10s 的写超时只覆盖拿到锁
// 之后的 ws.Write,救不回来。这里占住写槽位模拟"前一个写卡住",断言等待方在 ctx
// 超时内返回错误而不是永久阻塞。
//
// send 是全包唯一的写入口(sendMessage/Dispatch/心跳 ack/PushDesiredState 都经由
// 它),所以这一层的契约覆盖全部调用点,不需要为每个调用点重复一遍 10s 的慢测试。
func TestSendUnblocksWhenContextExpiresWhilePeerStalls(t *testing.T) {
	conn := newAgentConn(1, "fp", nil)

	// 占住槽位:等价于另一个 goroutine 正卡在 ws.Write 上。
	conn.writeSem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- conn.sendMessage(ctx, controlproto.TypeHeartbeatAck, "hb-1",
			controlproto.HeartbeatAckPayload{ServerTime: nowUTC().Format(time.RFC3339)})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("send must fail while the write slot is held")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("send error = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("send took %v; the wait was not bounded by ctx", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send never returned: the write wait is still unbounded (goroutine wedged)")
	}
}

// R3.3(AC2):超时返回的写方不能破坏槽位语义——它没拿到槽位就不该释放别人的槽位,
// 否则后续写会失去串行化保护。前序写完成后,新的写方必须能正常取得槽位。
func TestWriteSlotStaysConsistentAfterTimeout(t *testing.T) {
	conn := newAgentConn(1, "fp", nil)
	conn.writeSem <- struct{}{} // 前序写占住

	timedOut, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := conn.acquireWrite(timedOut); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquireWrite while held = %v, want DeadlineExceeded", err)
	}
	// 槽位仍被前序写持有:超时方不得把它误放掉。
	if len(conn.writeSem) != 1 {
		t.Fatalf("write slot occupancy = %d, want 1 (timed-out waiter must not release it)", len(conn.writeSem))
	}

	conn.releaseWrite() // 前序写结束

	fresh, cancelFresh := context.WithTimeout(context.Background(), time.Second)
	defer cancelFresh()
	if err := conn.acquireWrite(fresh); err != nil {
		t.Fatalf("acquireWrite after release = %v, want success", err)
	}
	conn.releaseWrite()
}

// R3(AC2):卡住的写方超时退出后不留 goroutine。断言写等待被 ctx 取消后
// goroutine 数回落,而不是随着每次超时写不断堆积。
func TestStalledWritesDoNotLeakGoroutines(t *testing.T) {
	conn := newAgentConn(1, "fp", nil)
	conn.writeSem <- struct{}{} // 全程占住,所有写都会超时

	settle := func() int {
		// 让已结束的 goroutine 退出调度后再采样。
		for i := 0; i < 20; i++ {
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
		return runtime.NumGoroutine()
	}
	before := settle()

	for i := 0; i < 25; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		if err := conn.acquireWrite(ctx); !errors.Is(err, context.DeadlineExceeded) {
			cancel()
			t.Fatalf("acquireWrite #%d = %v, want DeadlineExceeded", i, err)
		}
		cancel()
	}

	if after := settle(); after > before+5 {
		t.Fatalf("goroutines went from %d to %d; stalled writers are leaking", before, after)
	}
}
