package api

import (
	"net/http"
	"sync"
	"time"
)

// rateLimiter 是一个简单的按用户滑动窗口限流器。
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string][]time.Time // userID → request timestamps
	rpm     int                    // requests per minute (0 = unlimited)
	window  time.Duration
}

func newRateLimiter(rpm int) *rateLimiter {
	if rpm <= 0 {
		return &rateLimiter{rpm: 0}
	}
	return &rateLimiter{
		windows: make(map[string][]time.Time),
		rpm:     rpm,
		window:  time.Minute,
	}
}

// allow 在 userID 的请求应被允许时返回 true。
func (rl *rateLimiter) allow(userID string) bool {
	if rl.rpm <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// 清除过期条目。
	ts := rl.windows[userID]
	start := 0
	for start < len(ts) && ts[start].Before(cutoff) {
		start++
	}
	ts = ts[start:]

	if len(ts) >= rl.rpm {
		rl.windows[userID] = ts
		return false
	}
	rl.windows[userID] = append(ts, now)
	return true
}

// cleanup 定期清除过期条目。在 goroutine 中调用。
func (rl *rateLimiter) cleanup(interval time.Duration, done <-chan struct{}) {
	if rl.rpm <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.window)
			for uid, ts := range rl.windows {
				start := 0
				for start < len(ts) && ts[start].Before(cutoff) {
					start++
				}
				if start == len(ts) {
					delete(rl.windows, uid)
				} else {
					rl.windows[uid] = ts[start:]
				}
			}
			rl.mu.Unlock()
		}
	}
}

// rateLimitMiddleware 包装一个处理器，当用户超过配置的 RPM 时返回 429。
func rateLimitMiddleware(rl *rateLimiter, getUserID func(r *http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	if rl == nil || rl.rpm <= 0 {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		uid := getUserID(r)
		if !rl.allow(uid) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": map[string]string{
					"message": "rate limit exceeded — try again shortly",
					"type":    "rate_limit_error",
				},
			})
			return
		}
		next(w, r)
	}
}
