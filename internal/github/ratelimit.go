package github

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

type RateLimitTracker struct {
	mu        sync.Mutex
	remaining int
	resetAt   time.Time
	threshold int // back off when remaining drops below this
}

func NewRateLimitTracker(threshold int) *RateLimitTracker {
	return &RateLimitTracker{
		remaining: -1, // unknown until first response
		threshold: threshold,
	}
}

func (r *RateLimitTracker) Update(resp *http.Response) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			r.remaining = n
		}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			r.resetAt = time.Unix(epoch, 0)
		}
	}
}

func (r *RateLimitTracker) ShouldBackoff() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.remaining < 0 {
		return false // unknown, allow
	}
	return r.remaining <= r.threshold
}

func (r *RateLimitTracker) ResetAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resetAt
}

func (r *RateLimitTracker) Remaining() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remaining
}
