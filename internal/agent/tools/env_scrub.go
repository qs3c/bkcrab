package tools

import (
	"os"
	"strings"
)

// sensitiveEnvPrefixes 是标记的 NAME 前缀（不区分大小写）
// 环境变量仅作为运算符且不会被 LLM 驱动继承
// 子流程。 BKCLAW_STORAGE_* 和 BKCLAW_OBJECT_STORE_* 携带
// 守护进程的 DSN + 对象存储凭据； BKCLAW_SANDBOX_BOXLITE_*
// 携带沙箱提供商的 apikey。该模型没有业务
// 阅读其中任何一个。
var sensitiveEnvPrefixes = []string{
	"BKCLAW_STORAGE_",
	"BKCLAW_OBJECT_STORE_",
	"BKCLAW_SANDBOX_BOXLITE_",
	"AWS_",
	"GOOGLE_APPLICATION_CREDENTIALS",
}

// sensitiveEnvSubstrings 是不区分大小写的 NAME 子字符串，用于标记
// var 作为可能的秘密。故意比前缀列表更宽——我们
// 无法枚举每个操作员的定制环境，因此任何包含
// 这些被剥夺了。技能通过显式注入自己的env
// SkillEnvProvider，因此合法的技能凭证仍然可以达到
// 子进程；只有继承的父环境秘密才会被清除。
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

// isSensitiveEnvKey 报告是否给定的环境变量名称（no =value
// 后缀）应该在生成之前从父环境中删除
// LLM 代理可以驱动的子 shell。
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

// scrapSensitiveEnv 复制 env，并删除包含凭证的条目。
// 用于代理可以生成的每个子进程（exec / bash_session
// / host_exec) — 屏幕截图事件，其中一个喋喋不休的哄骗
// 模型进入`env | grep ALIYUN` 和 AccessKey + DSN 最终在
// 聊天回复证明“模型有壳”意味着“用户
// 在实践中具有所有环境变量”，无论系统如何提示
// 指导模型。
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

// buildSubprocessEnv 返回 LLM 驱动的子进程的完整环境：
// 清除敏感条目的父环境，然后按技能
// 覆盖应用于顶部，以便技能可以传递自己的（合法）
// 证书。调用者应该在任何地方使用它而不是让
// 默认为 os.Environ() 的裸继承。
func buildSubprocessEnv(skillEnv map[string]string) []string {
	return mergeEnv(scrubSensitiveEnv(os.Environ()), skillEnv)
}
