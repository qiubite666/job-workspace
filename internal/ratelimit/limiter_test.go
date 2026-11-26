package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWaitFirstCallImmediate 验证首次调用不等待。
func TestWaitFirstCallImmediate(t *testing.T) {
	l := New(20*time.Millisecond, 20*time.Millisecond)

	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("首次 Wait 返回错误: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Millisecond {
		t.Fatalf("首次调用应立即返回，实际耗时 %v", elapsed)
	}
}

// TestWaitSerialSpacing 验证连续 3 次调用至少产生 2 个间隔。
func TestWaitSerialSpacing(t *testing.T) {
	l := New(10*time.Millisecond, 20*time.Millisecond)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Wait 返回错误: %v", i+1, err)
		}
	}
	elapsed := time.Since(start)

	want := time.Duration(float64(2*10*time.Millisecond) * 0.8) // 2*minDelay*0.8
	if elapsed < want {
		t.Fatalf("3 次调用总耗时 %v 小于下限 %v（全局串行未生效）", elapsed, want)
	}
}

// TestWaitContextCancel 验证 ctx 取消能在等待中立即返回。
func TestWaitContextCancel(t *testing.T) {
	l := New(80*time.Millisecond, 80*time.Millisecond)

	// 首次调用立即占位，确保后续调用必然进入睡眠分支。
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("首次 Wait 返回错误: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := l.Wait(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望 context.DeadlineExceeded，实际 %v", err)
	}
	if elapsed > 60*time.Millisecond {
		t.Fatalf("取消后应立即返回，实际耗时 %v", elapsed)
	}

	// 已取消的 ctx 也必须立刻返回。
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	start = time.Now()
	if err := l.Wait(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("期望 context.Canceled，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("已取消的 ctx 应立即返回，实际耗时 %v", elapsed)
	}
}

// TestStats 验证计数与累计等待时长。
func TestStats(t *testing.T) {
	l := New(10*time.Millisecond, 10*time.Millisecond)

	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Wait 返回错误: %v", i+1, err)
		}
	}

	calls, total := l.Stats()
	if calls != 3 {
		t.Fatalf("calls 期望 3，实际 %d", calls)
	}
	if total <= 0 {
		t.Fatalf("totalWait 期望 > 0，实际 %v", total)
	}
	if minWant := time.Duration(float64(2*10*time.Millisecond) * 0.8); total < minWant {
		t.Fatalf("totalWait %v 小于下限 %v", total, minWant)
	}
}

// TestZeroDelayStillSerial 验证双 0 延迟退化为无等待但不 panic。
func TestZeroDelayStillSerial(t *testing.T) {
	l := New(0, 0)

	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Wait 返回错误: %v", i+1, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("零延迟不应产生等待，实际耗时 %v", elapsed)
	}

	calls, total := l.Stats()
	if calls != 5 {
		t.Fatalf("calls 期望 5，实际 %d", calls)
	}
	if total != 0 {
		t.Fatalf("totalWait 期望 0，实际 %v", total)
	}
}

// TestDelaySwap 验证 maxDelay < minDelay 时自动交换，不会 panic 且间隔不为负。
func TestDelaySwap(t *testing.T) {
	l := New(30*time.Millisecond, 5*time.Millisecond)
	if l.minDelay != 5*time.Millisecond || l.maxDelay != 30*time.Millisecond {
		t.Fatalf("延迟区间未交换: min=%v max=%v", l.minDelay, l.maxDelay)
	}
	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Wait 返回错误: %v", i+1, err)
		}
	}
}

// TestConcurrentSerial 验证并发调用下仍能保持串行（总耗时不小于 (n-1)*min）。
func TestConcurrentSerial(t *testing.T) {
	const n = 4
	l := New(10*time.Millisecond, 10*time.Millisecond)

	start := time.Now()
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			done <- l.Wait(context.Background())
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并发 Wait 返回错误: %v", err)
		}
	}
	elapsed := time.Since(start)

	want := time.Duration(float64((n-1)*10*time.Millisecond) * 0.8)
	if elapsed < want {
		t.Fatalf("并发 %d 次总耗时 %v 小于下限 %v", n, elapsed, want)
	}
	if calls, _ := l.Stats(); calls != n {
		t.Fatalf("calls 期望 %d，实际 %d", n, calls)
	}
}
