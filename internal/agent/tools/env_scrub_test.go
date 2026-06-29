package tools

import (
	"reflect"
	"sort"
	"testing"
)

func TestIsSensitiveEnvKey(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Operator-only prefixes — the screenshot incident.
		{"BKCRAB_STORAGE_DSN", true},
		{"BKCRAB_OBJECT_STORE_ACCESSKEY", true},
		{"BKCRAB_OBJECT_STORE_SECRETKEY", true},
		{"BKCRAB_OBJECT_STORE_BUCKET", true},
		{"BKCRAB_OBJECT_STORE_ALIYUN_INTERNAL", true},
		{"BKCRAB_SANDBOX_BOXLITE_URL", true},
		{"AWS_ACCESS_KEY_ID", true},
		{"AWS_SECRET_ACCESS_KEY", true},
		{"GOOGLE_APPLICATION_CREDENTIALS", true},

		// Substring matchers.
		{"ANTHROPIC_API_KEY", true},
		{"OPENAI_API_KEY", true},
		{"GITHUB_TOKEN", true},
		{"DB_PASSWORD", true},
		{"MYAPP_DSN", true},
		{"DATABASE_URL", true},
		{"SOMETHING_SECRET", true},
		{"PRIVATE_KEY", true},
		{"FOO_CREDENTIAL", true},
		{"apikey_lowercase", true}, // case-insensitive

		// Things that must NOT be scrubbed — needed by every child.
		{"PATH", false},
		{"HOME", false},
		{"USER", false},
		{"LANG", false},
		{"LC_ALL", false},
		{"TERM", false},
		{"PWD", false}, // present working dir, NOT a password
		{"BKCRAB_HOME", false},
		{"BKCRAB_LOG_LEVEL", false},
		{"BKCRAB_DEPLOY", false},
		{"BKCRAB_ALLOW_HOST_EXEC", false},
	}
	for _, c := range cases {
		got := isSensitiveEnvKey(c.name)
		if got != c.want {
			t.Errorf("isSensitiveEnvKey(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScrubSensitiveEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/x",
		"BKCRAB_STORAGE_DSN=postgres://user:pw@host/db",
		"BKCRAB_OBJECT_STORE_ACCESSKEY=AKIAEXAMPLE",
		"BKCRAB_OBJECT_STORE_SECRETKEY=secret",
		"ANTHROPIC_API_KEY=sk-ant-x",
		"AWS_ACCESS_KEY_ID=AKIA",
		"BKCRAB_HOME=/var/lib/bkcrab",
		"LANG=en_US.UTF-8",
		"PWD=/tmp", // must NOT match the PASSWD substring
	}
	got := scrubSensitiveEnv(in)
	sort.Strings(got)
	want := []string{
		"BKCRAB_HOME=/var/lib/bkcrab",
		"HOME=/home/x",
		"LANG=en_US.UTF-8",
		"PATH=/usr/bin",
		"PWD=/tmp",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scrubSensitiveEnv mismatch\n got:  %v\n want: %v", got, want)
	}
}

func TestBuildSubprocessEnvOverridesSkillKeys(t *testing.T) {
	// Make a minimal "parent env" by passing through scrubSensitiveEnv
	// and confirming a skill-provided FAL_KEY lands in the result even
	// though FAL_KEY would match the _API_KEY substring at first glance.
	// (FAL_KEY itself doesn't match any pattern — that's fine; this test
	// just confirms skill env wins over a parent that already has the
	// same key.)
	t.Setenv("FAL_KEY", "from-parent")
	t.Setenv("BKCRAB_STORAGE_DSN", "must-be-stripped")
	out := buildSubprocessEnv(map[string]string{"FAL_KEY": "from-skill"})

	var sawFalKey, sawDSN bool
	for _, kv := range out {
		if kv == "FAL_KEY=from-skill" {
			sawFalKey = true
		}
		if len(kv) >= len("BKCRAB_STORAGE_DSN=") && kv[:len("BKCRAB_STORAGE_DSN=")] == "BKCRAB_STORAGE_DSN=" {
			sawDSN = true
		}
	}
	if !sawFalKey {
		t.Errorf("expected skill env FAL_KEY to win over parent; not found in %v", out)
	}
	if sawDSN {
		t.Errorf("BKCRAB_STORAGE_DSN must be scrubbed; leaked in %v", out)
	}
}
