package imagegen

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	redis "github.com/redis/go-redis/v9"
)

func TestProviderLimiterKeyIsBoundedAndIsolatesProviders(t *testing.T) {
	alpha, err := ProviderLimiterKey("test:", "alpha", "model/a")
	if err != nil {
		t.Fatalf("alpha key: %v", err)
	}
	beta, err := ProviderLimiterKey("test:", "beta", "model/a")
	if err != nil {
		t.Fatalf("beta key: %v", err)
	}
	otherModel, err := ProviderLimiterKey("test:", "alpha", "model/b")
	if err != nil {
		t.Fatalf("model key: %v", err)
	}
	if alpha == beta || alpha == otherModel || !strings.Contains(alpha, "image:provider:alpha:") || len(alpha) > 512 {
		t.Fatalf("key isolation/bound: alpha=%q beta=%q model=%q", alpha, beta, otherModel)
	}
	for _, invalid := range [][2]string{{"", "model"}, {"../provider", "model"}, {"alpha", "bad\nmodel"}} {
		if _, err := ProviderLimiterKey("test:", invalid[0], invalid[1]); err == nil {
			t.Fatalf("invalid provider/model accepted: %#v", invalid)
		}
	}
}

func TestProviderLimiterRedisIntegration(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("BKCRAB_TEST_REDIS_ADDR"))
	if address == "" {
		t.Skip("BKCRAB_TEST_REDIS_ADDR is not set")
	}
	client := redis.NewClient(&redis.Options{
		Addr: address, Password: os.Getenv("BKCRAB_TEST_REDIS_PASSWORD"), ContextTimeoutEnabled: true,
	})
	defer client.Close()
	prefix := fmt.Sprintf("bkcrab:test:imagegen:%d:", time.Now().UnixNano())
	limiter, err := NewRedisProviderLimiter(client, prefix)
	if err != nil {
		t.Fatalf("new limiter: %v", err)
	}
	ctx := context.Background()
	first, ok, err := limiter.Acquire(ctx, "openai", "gpt-image-1", "token-000000000001", 2, 300*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("first acquire: lease=%#v ok=%v err=%v", first, ok, err)
	}
	second, ok, err := limiter.Acquire(ctx, "openai", "gpt-image-1", "token-000000000002", 2, 300*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("second acquire: lease=%#v ok=%v err=%v", second, ok, err)
	}
	if _, ok, err := limiter.Acquire(ctx, "openai", "gpt-image-1", "token-000000000003", 2, 300*time.Millisecond); err != nil || ok {
		t.Fatalf("limit exceeded acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := limiter.Acquire(ctx, "fal", "flux", "token-000000000004", 1, 300*time.Millisecond); err != nil || !ok {
		t.Fatalf("different provider not isolated: ok=%v err=%v", ok, err)
	}

	wrong := first
	wrong.Token = "token-999999999999"
	if err := limiter.Release(ctx, wrong); err != nil {
		t.Fatalf("wrong-token release: %v", err)
	}
	if _, ok, err := limiter.Acquire(ctx, "openai", "gpt-image-1", "token-000000000005", 2, 300*time.Millisecond); err != nil || ok {
		t.Fatalf("wrong token released another lease: ok=%v err=%v", ok, err)
	}
	if err := limiter.Release(ctx, first); err != nil {
		t.Fatalf("release first: %v", err)
	}
	if err := limiter.Release(ctx, first); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
	replacement, ok, err := limiter.Acquire(ctx, "openai", "gpt-image-1", "token-000000000006", 2, 120*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("replacement acquire: ok=%v err=%v", ok, err)
	}
	if err := limiter.Renew(ctx, replacement, 300*time.Millisecond); err != nil {
		t.Fatalf("renew: %v", err)
	}
	time.Sleep(160 * time.Millisecond)
	if _, ok, err := limiter.Acquire(ctx, "openai", "gpt-image-1", "token-000000000007", 2, 120*time.Millisecond); err != nil || ok {
		t.Fatalf("renew did not retain capacity: ok=%v err=%v", ok, err)
	}
	time.Sleep(180 * time.Millisecond)
	if _, ok, err := limiter.Acquire(ctx, "openai", "gpt-image-1", "token-000000000008", 2, 120*time.Millisecond); err != nil || !ok {
		t.Fatalf("expired token did not free capacity: ok=%v err=%v", ok, err)
	}
	_ = limiter.Release(ctx, second)
	_ = limiter.Release(ctx, replacement)
}
