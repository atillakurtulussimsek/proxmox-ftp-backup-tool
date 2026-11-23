package ftpclient

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func copyWithRate(t *testing.T, size int, rate RateFunc) time.Duration {
	t.Helper()
	ctx := WithRateLimit(context.Background(), rate)
	t0 := time.Now()
	n, err := copyCtx(ctx, io.Discard, bytes.NewReader(make([]byte, size)), nil)
	if err != nil || n != int64(size) {
		t.Fatalf("kopyalama: %d, %v", n, err)
	}
	return time.Since(t0)
}

func TestRateLimit(t *testing.T) {
	// 2 MB, 4 MB/sn → yaklaşık 0,5 sn
	d := copyWithRate(t, 2_000_000, func() int64 { return 4_000_000 })
	if d < 400*time.Millisecond || d > 900*time.Millisecond {
		t.Fatalf("süre %v, ~500ms bekleniyordu", d)
	}
}

func TestRateLimitUnlimited(t *testing.T) {
	if d := copyWithRate(t, 20_000_000, func() int64 { return 0 }); d > 200*time.Millisecond {
		t.Fatalf("sınırsızda süre %v", d)
	}
}

func TestRateLimitCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	ctx = WithRateLimit(ctx, func() int64 { return 100_000 })
	_, err := copyCtx(ctx, io.Discard, bytes.NewReader(make([]byte, 10_000_000)), nil)
	if err == nil {
		t.Fatal("iptal hatası bekleniyordu")
	}
}
