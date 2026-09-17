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
		digest, err := hex.DecodeString(key[strings.LastIndex(key, ":")+1:])
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
	if got := c.key(Key("health")); got != "bkcrab:agentctx:v3:health" {
		t.Fatalf("health key = %q", got)
	}
	if Key("health", "scope") == Key("health") {
		t.Fatal("scoped key collided with health probe")
	}
}

func TestFileAndSkillCatalogLabels(t *testing.T) {
	for _, tc := range []struct {
		parts  []string
		prefix string
	}{
		{[]string{"file", "a", "u", "USER.md"}, "file:USER.md:"},
		{[]string{"file", "a", "u", "MEMORY.md"}, "file:MEMORY.md:"},
		{[]string{"file", "a", "u", "notes/a:b %.md"}, "file:notes%2Fa%3Ab+%25.md:"},
		{[]string{"skillcatalog", "_global", "", ""}, "skillcatalog:global:"},
		{[]string{"skillcatalog", "_user_u1", "", ""}, "skillcatalog:user:"},
		{[]string{"skillcatalog", "agt_1", "", ""}, "skillcatalog:agent:"},
	} {
		if key := Key(tc.parts...); !strings.HasPrefix(key, tc.prefix) {
			t.Fatalf("key %q does not start with %q", key, tc.prefix)
		}
	}
	if Key("file", "a", "u", "a:b") == Key("file", "a", "u", "a%3Ab") {
		t.Fatal("escaped and literal filenames collided")
	}
	if Key("file", "a", "u1", "USER.md") == Key("file", "a", "u2", "USER.md") {
		t.Fatal("same filename in different user scopes collided")
	}
	if Key("skillcatalog", "_user_u1", "", "") == Key("skillcatalog", "_user_u2", "", "") {
		t.Fatal("user skill catalogs collided")
	}
}
