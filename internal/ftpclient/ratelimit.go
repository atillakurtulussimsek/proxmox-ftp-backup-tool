package ftpclient

import (
	"context"
	"sync"
	"time"
)

// RateFunc, anlık hız sınırını bayt/sn olarak döndürür; 0 sınırsızdır. Aktarım sürerken
// ayar değişirse yeni değer en geç RATE_REFRESH sonra uygulanır.
type RateFunc func() int64

const RATE_REFRESH = time.Second

type rateKey struct{}

// WithRateLimit, bu bağlamla yapılan Upload/Download aktarımlarına hız sınırı ekler.
func WithRateLimit(ctx context.Context, rate RateFunc) context.Context {
	if rate == nil {
		return ctx
	}
	return context.WithValue(ctx, rateKey{}, rate)
}

// limiter, basit bir token bucket'tır. Tek aktarım içinde kullanılır.
type limiter struct {
	mu      sync.Mutex
	rateFn  RateFunc
	rate    int64
	checked time.Time
	tokens  float64
	last    time.Time
}

func limiterFrom(ctx context.Context) *limiter {
	fn, _ := ctx.Value(rateKey{}).(RateFunc)
	if fn == nil {
		return nil
	}
	return &limiter{rateFn: fn}
}

// wait, n bayt aktarıldıktan sonra sınırı aşmamak için gereken süre kadar bekler.
func (l *limiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	now := time.Now()
	if l.checked.IsZero() || now.Sub(l.checked) >= RATE_REFRESH {
		l.rate = l.rateFn()
		l.checked = now
	}
	if l.rate <= 0 {
		l.tokens, l.last = 0, now
		l.mu.Unlock()
		return nil
	}
	burst := float64(l.rate) / 4
	if !l.last.IsZero() {
		l.tokens += now.Sub(l.last).Seconds() * float64(l.rate)
		if l.tokens > burst {
			l.tokens = burst
		}
	}
	l.last = now
	l.tokens -= float64(n)
	var d time.Duration
	if l.tokens < 0 {
		d = time.Duration(-l.tokens / float64(l.rate) * float64(time.Second))
	}
	l.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
