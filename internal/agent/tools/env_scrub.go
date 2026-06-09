package tools

import (
	"os"
	"strings"
)

// sensitiveEnvPrefixes are NAME prefixes (case-insensitive) that mark
// an env var as operator-only and never inherited by LLM-driven
// subprocesses. FASTCLAW_STORAGE_* and FASTCLAW_OBJECT_STORE_* carry
// the daemon's DSN + object-store credentials; FASTCLAW_SANDBOX_BOXLITE_*
// carries the sandbox provider's apikey. The model has no business
// reading any of these.
var sensitiveEnvPrefixes = []string{
	"FASTCLAW_STORAGE_",
	"FASTCLAW_OBJECT_STORE_",
	"FASTCLAW_SANDBOX_BOXLITE_",
	"AWS_",
	"GOOGLE_APPLICATION_CREDENTIALS",
}

// sensitiveEnvSubstrings are case-insensitive NAME substrings that mark
// a var as likely-secret. Deliberately wider than the prefix list — we
// can't enumerate every operator's bespoke env, so any name containing
// these is stripped. Skills inject their own env explicitly through
// SkillEnvProvider, so legitimate skill credentials still reach the
// child process; only INHERITED parent-env secrets get scrubbed.
var sensitiveEnvSubstrings = []string{
	"SECRET",
	"TOKEN",
	"PASSWORD",
	"PASSWD",
	"CREDENTIAL",
	"PRIVATE_KEY",
	"_API_KEY",
	"APIKEY",
	"ACCESS_KEY",
	"ACCESSKEY",
	"SECRET_KEY",
	"SECRETKEY",
	"_DSN",
	"DATABASE_URL",
}

// isSensitiveEnvKey reports whether the given env-var NAME (no =value
// suffix) should be stripped from the parent env before spawning a
// child shell the LLM agent can drive.
func isSensitiveEnvKey(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range sensitiveEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, sub := range sensitiveEnvSubstrings {
		if strings.Contains(upper, sub) {
			return true
		}
	}
	return false
}

// scrubSensitiveEnv copies env with credential-bearing entries removed.
// Use for every child process the agent can spawn (exec / bash_session
// / host_exec) — the screenshot incident where a chatter coaxed the
// model into `env | grep ALIYUN` and the AccessKey + DSN ended up in
// the chat reply proved that "the model has shell" implies "the user
// has all env vars" in practice, regardless of how the system prompt
// instructs the model.
func scrubSensitiveEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		idx := strings.IndexByte(kv, '=')
		name := kv
		if idx >= 0 {
			name = kv[:idx]
		}
		if isSensitiveEnvKey(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// buildSubprocessEnv returns the full env for an LLM-driven child:
// the parent env with sensitive entries scrubbed, then per-skill
// overrides applied on top so skills can pass their own (legitimate)
// credentials. Callers should use this everywhere instead of letting
// Go default to bare inheritance of os.Environ().
func buildSubprocessEnv(skillEnv map[string]string) []string {
	return mergeEnv(scrubSensitiveEnv(os.Environ()), skillEnv)
}
