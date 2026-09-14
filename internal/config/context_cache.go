package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/contextcache"
)

type EnvContextCache struct {
	Enabled       bool
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	Prefix        string
	TTLSeconds    int
	TimeoutMS     int
	err           error
}

func loadContextCacheEnv() EnvContextCache {
	c := EnvContextCache{RedisAddr: os.Getenv("BKCRAB_CONTEXT_CACHE_REDIS_ADDR"), RedisPassword: os.Getenv("BKCRAB_CONTEXT_CACHE_REDIS_PASSWORD"), Prefix: os.Getenv("BKCRAB_CONTEXT_CACHE_PREFIX"), TTLSeconds: int(contextcache.DefaultTTL / time.Second), TimeoutMS: 200}
	if s := os.Getenv("BKCRAB_CONTEXT_CACHE_ENABLED"); s != "" {
		v, e := strconv.ParseBool(s)
		c.Enabled = v
		if e != nil {
			c.err = fmt.Errorf("invalid context cache enabled flag")
		}
	}
	for _, item := range []struct {
		name string
		p    *int
	}{{"REDIS_DB", &c.RedisDB}, {"TTL_SECONDS", &c.TTLSeconds}, {"TIMEOUT_MS", &c.TimeoutMS}} {
		if s := os.Getenv("BKCRAB_CONTEXT_CACHE_" + item.name); s != "" {
			v, e := strconv.Atoi(s)
			if e != nil {
				c.err = fmt.Errorf("invalid context cache %s", item.name)
			} else {
				*item.p = v
			}
		}
	}
	return c
}
func (c EnvContextCache) Validate() error {
	if c.err != nil {
		return c.err
	}
	if !c.Enabled {
		return nil
	}
	if c.RedisDB < 0 || c.TTLSeconds < 0 || c.TimeoutMS < 0 {
		return fmt.Errorf("invalid context cache duration or Redis DB")
	}
	if c.Prefix != "" && !strings.HasPrefix(c.Prefix, "bkcrab:agentctx:") {
		return fmt.Errorf("context cache prefix must start with bkcrab:agentctx:")
	}
	return c.Config().Validate()
}
func (c EnvContextCache) Config() contextcache.Config {
	return contextcache.Config{Addr: c.RedisAddr, Password: c.RedisPassword, DB: c.RedisDB, Prefix: c.Prefix, TTL: time.Duration(c.TTLSeconds) * time.Second, Timeout: time.Duration(c.TimeoutMS) * time.Millisecond}
}

// String never reveals credentials when deployment configuration is logged.
func (c EnvContextCache) String() string {
	return fmt.Sprintf("contextCache{enabled:%t addr:%q db:%d}", c.Enabled, c.RedisAddr, c.RedisDB)
}
