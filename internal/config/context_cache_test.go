package config

import (
	"strings"
	"testing"
	"time"
)

func TestContextCacheEnvAndSecret(t *testing.T) {
	t.Setenv("BKCRAB_CONTEXT_CACHE_ENABLED", "true")
	t.Setenv("BKCRAB_CONTEXT_CACHE_REDIS_ADDR", "localhost:16389")
	t.Setenv("BKCRAB_CONTEXT_CACHE_REDIS_PASSWORD", "do-not-log-this")
	t.Setenv("BKCRAB_CONTEXT_CACHE_TTL_SECONDS", "")
	c := LoadEnv().ContextCache
	if c.Config().TTL != 30*time.Minute {
		t.Fatalf("default TTL = %s", c.Config().TTL)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.String(), "do-not-log-this") {
		t.Fatal("secret leaked")
	}
	t.Setenv("BKCRAB_CONTEXT_CACHE_REDIS_DB", "garbage")
	if err := LoadEnv().ContextCache.Validate(); err == nil {
		t.Fatal("invalid DB accepted")
	}
}
