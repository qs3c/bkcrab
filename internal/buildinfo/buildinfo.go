// Package buildinfo 保存二进制文件的构建标识（版本、提交、日期），
// 通过链接时的 -ldflags -X 填充。独立成包（而非放在 cmd/bkclaw/main）
// 是为了让 internal/* 代码——尤其是 agent 系统提示构建器——可以读取它，
// 而不会将 cmd 包引入依赖循环。
package buildinfo

import (
	"os"
	"strings"
)

// Version 是 BkClaw 的发布标签（例如 "v0.4.2"），由 Makefile 通过
// `git describe --tags` 设置。未传递 ldflag 的临时 `go build` 默认为
// "dev"；调用方应将其视为"无正式版本"而非真正的发布版本。
var Version = "dev"

// Commit 是构建源代码树的短 git SHA。
var Commit = "unknown"

// Date 是二进制文件构建的 UTC 时间戳。
var Date = "unknown"

// IsHostedDeploy 报告当前 bkclaw 进程是否运行在托管/多租户部署（云）
// 环境，而非自托管单操作员安装。由 BKCLAW_DEPLOY 环境变量控制：
//
//	BKCLAW_DEPLOY=hosted        → IsHostedDeploy() == true
//	BKCLAW_DEPLOY=self-hosted   → false
//	（未设置或其它值）           → false（默认 = 自托管）
//
// 操作员在其云部署清单中设置此值（k8s values.yaml、docker-compose env 等）。
// 默认自托管匹配最常见的情况（开发者在自己的笔记本上运行 bkclaw），
// 也避免了操作员忘记在云部署上设置环境变量时出现意外的升级提示——
// 最好是默认"告诉用户如何升级"，仅在显式选择加入时抑制。
//
// 每次调用都重新读取（不缓存），以便配置编辑 + sighup 流程可以在
// 不重启进程的情况下切换它，尽管实际上它只在启动时设置一次。
func IsHostedDeploy() bool {
	return osDeployVar() == "hosted"
}

// osDeployVar 读取 BKCLAW_DEPLOY 并归一化。小写化处理，使大小写
// 变体不会静默绕过 hosted 标志。
func osDeployVar() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv("BKCLAW_DEPLOY")))
}

// IsHostExecAllowed 报告 agent 运行时是否应注册 `host_exec` 逃生舱工具。
// 仅当操作员通过 BKCLAW_ALLOW_HOST_EXEC=1（或 "true"/"yes"）显式选择加入，
// 且不是托管多租户部署时才返回 true。
//
// 默认关闭。早期版本为每个自托管安装注册 host_exec——这是一个过于宽松
// 的默认值，会将操作员的主机 shell 暴露给任何 agent 可达的外部 IM 用户
// （微信、Discord、飞书等）。有了新的门控，实际需要 host_exec 的操作员
// （单用户笔记本开发流程、`bkclaw upgrade`、`~/Downloads` 整理）在部署时
// 设置环境变量；其他人则获得更安全的沙箱或无沙箱行为。
func IsHostExecAllowed() bool {
	if IsHostedDeploy() {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("BKCLAW_ALLOW_HOST_EXEC")))
	return v == "1" || v == "true" || v == "yes"
}
