// Package ratelimit 提供跨 goroutine 的全局串行限速器。
//
// 设计目标：招聘平台的列表页接口对「单位时间内的请求数」敏感，且并发请求容易
// 触发风控。Limiter 让所有调用排成一条队，任意两次实际放行之间至少间隔一个
// [minDelay, maxDelay] 区间内的随机时长，既有下限也有抖动，避免机器节奏。
package ratelimit

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// Limiter 是全局串行的请求节流器，可被多个 goroutine 同时使用。
//
// 内部只用一个互斥锁「预占槽位」：每次 Wait 在锁内算出本次应放行的时刻并
// 立刻把「下一个槽位」推后，真正的等待发生在锁外，因此睡眠中的 goroutine
// 不会阻塞 Stats 或其他调用者的排队。
type Limiter struct {
	mu        sync.Mutex
	minDelay  time.Duration
	maxDelay  time.Duration
	next      time.Time // 下一个槽位允许放行的最早时刻
	started   bool      // 是否已有调用占过槽位
	calls     int       // Wait 调用次数（含被取消的调用）
	totalWait time.Duration
	rnd       *rand.Rand
	nowFn     func() time.Time // 便于测试注入时钟
}

// New 创建限速器。
//
// maxDelay < minDelay 时两者自动交换；两者都为 0（或负数被归一为 0）时退化为
// 「不等待但仍串行」，便于 sample/local 模式关闭节流而不改调用方代码。
func New(minDelay, maxDelay time.Duration) *Limiter {
	if minDelay < 0 {
		minDelay = 0
	}
	if maxDelay < 0 {
		maxDelay = 0
	}
	if maxDelay < minDelay {
		minDelay, maxDelay = maxDelay, minDelay
	}
	return &Limiter{
		minDelay: minDelay,
		maxDelay: maxDelay,
		rnd:      rand.New(rand.NewSource(time.Now().UnixNano())),
		nowFn:    time.Now,
	}
}

// Wait 阻塞到本调用被允许放行；ctx 被取消时立即返回 ctx.Err()。
//
// 首次调用立即返回（等待 0），之后每次调用至少与前一次放行间隔
// [minDelay, maxDelay] 内的随机值。被取消的调用也会占掉一个槽位，
// 以保证「全局串行」不被破坏。
func (l *Limiter) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	l.mu.Lock()
	now := l.nowFn()
	var sleep time.Duration
	if !l.started {
		l.started = true
		l.next = now.Add(l.randDelay())
	} else {
		target := l.next
		if target.After(now) {
			sleep = target.Sub(now)
		}
		// 抢占下一个槽位：基准取「原定放行时刻」与「当前时刻」中较晚者。
		base := target
		if now.After(base) {
			base = now
		}
		l.next = base.Add(l.randDelay())
	}
	l.calls++
	l.mu.Unlock()

	if sleep > 0 {
		if err := sleepCtx(ctx, sleep); err != nil {
			return err
		}
		l.mu.Lock()
		l.totalWait += sleep
		l.mu.Unlock()
	}
	return nil
}

// Stats 返回累计的 Wait 调用次数与实际等待总时长。
//
// totalWait 只统计真正发生睡眠的部分，首次调用以及无需等待的调用不计入。
func (l *Limiter) Stats() (calls int, totalWait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls, l.totalWait
}

// randDelay 在 [minDelay, maxDelay] 中取一个随机间隔；调用方需持有锁。
func (l *Limiter) randDelay() time.Duration {
	if l.maxDelay <= 0 {
		return 0
	}
	span := l.maxDelay - l.minDelay
	if span <= 0 {
		return l.minDelay
	}
	return l.minDelay + time.Duration(l.rnd.Int63n(int64(span)+1))
}

// sleepCtx 是可被 ctx 打断的睡眠。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
