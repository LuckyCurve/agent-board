package main

import (
	"sync"
	"time"
)

// loginLimiter 按来源地址记失败次数；窗口内超过上限就拒绝，成功登录清零。
type loginLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	now    func() time.Time
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = time.Minute
	}
	return &loginLimiter{
		hits:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
		now:    time.Now,
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
	times := l.recent(key)
	if len(times) < l.limit {
		return false, 0
	}
	wait := l.window - l.now().Sub(times[0])
	if wait < 0 {
		wait = 0
	}
	return true, wait
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[key] = append(l.recent(key), l.now())
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
