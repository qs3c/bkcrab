package contextcache

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestReadableKeysPreserveScopeIsolation(t *testing.T) {
	seen := make(map[string]bool)
	for _, parts := range [][]string{
		{"session", "u", "a", "s"},
		{"session", "a", "u", "s"},
		{"session", "u:a", "b", "s"},
		{"session", "u", "a:b", "s"},
		{"file", "a", "u", "USER.md"},
		{"file", "a", "u", "MEMORY.md"},
		{"skillcatalog", "a", "", ""},
		{"skillstate", "a", "", ""},
		{"agent", "a", "", ""},
	} {
		key := Key(parts...)
		prefix := parts[0] + ":"
		if !strings.HasPrefix(key, prefix) {
			t.Fatalf("key %q is missing resource type %q", key, prefix)
		}
		digest, err := hex.DecodeString(strings.TrimPrefix(key, prefix))
		if err != nil || len(digest) != 32 {
			t.Fatalf("key %q must keep scope identifiers in a SHA-256 digest", key)
		}
		if seen[key] || Key(parts...) != key {
			t.Fatalf("unstable or colliding scope key %q", key)
		}
		seen[key] = true
	}
}

func TestDefaultNamespaceAndHealthKey(t *testing.T) {
	c, err := New(Config{Addr: "localhost:6379"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.key(Key("health")); got != "bkcrab:agentctx:v2:health" {
		t.Fatalf("health key = %q", got)
	}
	if Key("health", "scope") == Key("health") {
		t.Fatal("scoped key collided with health probe")
	}
}
