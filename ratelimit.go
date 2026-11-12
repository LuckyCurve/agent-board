package main

import (
	"sync"
	"time"
)

// loginLimiter 按来源地址记失败次数；窗口内超过上限就拒绝，成功登录清零。
// 另外有一个全局桶：分布式的换 IP 爆破也要拦住，但阈值必须远高于单 IP，
// 免得别人打满自己的桶就把主人一起锁在门外。
type loginLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	now    func() time.Time

	// 全局桶：分布式的换 IP 爆破也要拦住。阈值给得很宽——单 IP 的桶已经挡住
	// 正常爆破，全局桶只是兜底，不能让人靠打满它把主人一起锁在门外。
	global     []time.Time
	globalMax  int
	globalSpan time.Duration
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = time.Minute
	}
	return &loginLimiter{
		hits:       make(map[string][]time.Time),
		limit:      limit,
		window:     window,
		now:        time.Now,
		globalMax:  3000,
		globalSpan: 10 * time.Minute,
	}
}

func (l *loginLimiter) recent(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	times := l.hits[key]
	i := 0
	for i < len(times) && !times[i].After(cutoff) {
		i++
	}
	if i > 0 {
		times = append([]time.Time(nil), times[i:]...)
	}
	if len(times) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = times
	}
	return times
}

// status 返回是否应拒绝以及剩余等待时间。
func (l *loginLimiter) status(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if wait, blocked := l.blockedByGlobal(now); blocked {
		return true, wait
	}
	times := l.recent(key)
	if len(times) < l.limit {
		return false, 0
	}
	wait := l.window - now.Sub(times[0])
	if wait < 0 {
		wait = 0
	}
	return true, wait
}

func (l *loginLimiter) blockedByGlobal(now time.Time) (time.Duration, bool) {
	cutoff := now.Add(-l.globalSpan)
	i := 0
	for i < len(l.global) && !l.global[i].After(cutoff) {
		i++
	}
	if i > 0 {
		l.global = append([]time.Time(nil), l.global[i:]...)
	}
	if l.globalMax <= 0 || len(l.global) < l.globalMax {
		return 0, false
	}
	wait := l.globalSpan - now.Sub(l.global[0])
	if wait < 0 {
		wait = 0
	}
	return wait, true
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.hits[key] = append(l.recent(key), now)
	l.global = append(l.global, now)
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
