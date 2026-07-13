package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/buildinfo"
	"github.com/qs3c/bkcrab/internal/sandbox"
	"github.com/qs3c/bkcrab/internal/skills"
)

type execArgs struct {
	Command         string `json:"command"`
	Stdin           string `json:"stdin,omitempty"`             // optional: piped to the command's stdin
	Timeout         int    `json:"timeout,omitempty"`           // seconds, default 120
	Sandbox         bool   `json:"sandbox,omitempty"`           // force sandbox for this call
	RunInBackground bool   `json:"run_in_background,omitempty"` // launch detached, return bash_id for bash_output / kill_shell
}

func rejectLearnerManagedCommand(command string) error {
	// This is a guardrail against accidental direct shell edits, not an OS
	// security boundary: an unrestricted host shell can construct the path via
	// variables, scripts, or symlinks. Strong isolation requires sandbox mode,
	// where the agent home/learner cache is not mounted writable.
	normalized := strings.ToLower(strings.ReplaceAll(command, `\`, "/"))
	if strings.Contains(normalized, strings.ToLower(skills.LearnerSkillsDirName)) {
		return fmt.Errorf("%s", LearnerSkillsFileRefusal)
	}
	return nil
}

// MetaSandboxPrefix 将执行结果标记为已在沙箱内运行。
// 放置在第一行，以便代理循环可以将其提取到
// tool_result 事件元数据并将其从模型看到的内容中剥离。
// 使用 ASCII 单位分隔符，因此它永远不会与 shell 输出冲突。
const MetaSandboxPrefix = "\x1fFC_META:sandbox\x1f\n"

var dangerousCommands = []string{
	"rm -rf /",
	"mkfs",
	"dd if=",
	":(){:|:&};:",
	"> /dev/sda",
}

// SandboxConfig 保存传递给 exec 工具注册的沙箱设置。
type SandboxConfig struct {
	Enabled   bool
	Image     string
	Pool      *sandbox.SandboxPool
	Workspace string
	AgentID   string
	Policy    *sandbox.Policy
}

// SkillEnvProvider 按名称返回技能的环境变量。
type SkillEnvProvider func(skillName string) map[string]string

func registerExec(r *Registry) {
	registerExecWithSandbox(r, nil)
}

func registerExecWithSandbox(r *Registry, sbCfg *SandboxConfig) {
	registerExecFull(r, sbCfg, nil, nil)
}

// RegisterExecWithSkillEnv 注册具有技能环境注入支持的 exec 工具。
// 在注册表上缓存 envProvider + SkillDirs，以便稍后使用 SetExecutor
// （每会话沙箱绑定）可以重新应用 env 注入
// 重新注册 exec 闭包——否则像 image-tool run 这样的技能
// 在没有 FAL_KEY / REPLICATE_API_TOKEN 的容器中。
func RegisterExecWithSkillEnv(r *Registry, sbCfg *SandboxConfig, envProvider SkillEnvProvider, skillDirs []string) {
	r.envProvider = envProvider
	r.skillDirs = skillDirs
	registerExecFull(r, sbCfg, envProvider, skillDirs)
}

func registerExecFull(r *Registry, sbCfg *SandboxConfig, envProvider SkillEnvProvider, skillDirs []string) {
	r.Register("exec", "Execute a shell command and return stdout/stderr. For binary or image output (PNG, JPEG, PDF, audio, video), write the file into the workspace (e.g. ./out.png) and reference it by relative path in your reply — do NOT base64-encode it into stdout, and do NOT inline data: URLs in your response. The workspace file will be surfaced to the user via the Files panel.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"stdin": map[string]interface{}{
				"type":        "string",
				"description": "Optional input piped to the command's stdin. Use this to feed JSON args to a skill script: command='python /skills/x/main.py', stdin='{\"prompt\":\"...\"}'.",
			},
			"timeout": map[string]interface{}{
				"type":        "integer",
				"description": "Timeout in seconds (default 120). Headless-browser workflows (camoufox-cli) need a longer ceiling for the first call — the daemon + browser cold-start can take 2-3 min when traffic is proxied; subsequent calls in the same session are sub-second.",
			},
			"sandbox": map[string]interface{}{
				"type":        "boolean",
				"description": "Force execution in sandbox container",
			},
			"run_in_background": map[string]interface{}{
				"type":        "boolean",
				"description": "Launch the command in the background and return a bash_id immediately. Use this for long-running processes (dev servers, build watchers, migrations). Read output later via bash_output(bash_id); terminate via kill_shell(bash_id). Background sessions live until killed or the agent shuts down.",
			},
		},
		"required": []string{"command"},
	}, makeExecToolFull(r, sbCfg, envProvider, skillDirs))
}

func makeExecTool(sbCfg *SandboxConfig) ToolFunc {
	return makeExecToolFull(nil, sbCfg, nil, nil)
}

// makeExecToolFull 捕获注册表指针，以便它可以查阅
// 调用时的运行时 `sandboxRequired` 标志 - 这就是合同
// SetSandboxRequired 在任意层配置沙箱时发布
// 向上堆栈，即使它在代理构建时关闭。没有这个，
// 在bindSession期间`pool.Get()`失败会悄悄地泄漏到
// 主机外壳。
func makeExecToolFull(r *Registry, sbCfg *SandboxConfig, envProvider SkillEnvProvider, skillDirs []string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args execArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		if args.Command == "" {
			return "", fmt.Errorf("command is required")
		}
		if err := rejectLearnerManagedCommand(args.Command); err != nil {
			return "", err
		}

		// 检查危险命令
		lower := strings.ToLower(args.Command)
		for _, dc := range dangerousCommands {
			if strings.Contains(lower, dc) {
				return "", fmt.Errorf("dangerous command blocked: %s", args.Command)
			}
		}

		timeout := 120
		if args.Timeout > 0 {
			timeout = args.Timeout
		}

		execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()

		// 如果提供了 stdin，则在前面添加一个定界符样式的管道，以便
		// 现有的单字符串执行路径提供它。引用“EOF”
		// 禁用heredoc主体内的变量扩展，因此JSON
		// 有效负载不会被意外重写。
		command := args.Command
		if args.Stdin != "" {
			command = fmt.Sprintf("(cat <<'__FCSTDIN__'\n%s\n__FCSTDIN__\n) | %s", args.Stdin, args.Command)
		}

		// 如果启用或强制使用沙箱。注册表的
		// sandboxRequired 标志涵盖了运行时决定的情况
		// 施工后必须使用沙箱（需要兄弟代理
		// 它，或管理员在流程中翻转了settings.sandbox.enabled）。
		useSandbox := args.Sandbox || (sbCfg != nil && sbCfg.Enabled) || (r != nil && r.sandboxRequired)

		// 在 v1 中，后台模式仅适用于主机。沙盒模式背景
		// 在 sandbox.Executor 上需要 StartBackground / Poll / Kill
		// （或每个后端 tmux-inside-container 管道）——两者都是
		// 后续行动。在那之前，将模型指向 tmux 作为
		// 解决方法，使其在沙箱内有一条前进的路径。
		if args.RunInBackground {
			if useSandbox {
				return "", fmt.Errorf("run_in_background is not yet supported in sandbox mode — start the long-running command via tmux inside the sandbox instead, e.g. exec({command: \"tmux new-session -d -s job '<your command>'\"}) then exec({command: \"tmux capture-pane -t job -p\"}) to read output and exec({command: \"tmux kill-session -t job\"}) to stop")
			}
			if r == nil || r.shellMgr == nil {
				return "", fmt.Errorf("run_in_background unavailable: shell manager not initialised")
			}
			// 始终传递一个显式的（经过清理的）环境，以便子进程
			// 永远不会继承原始 os.Environ() — 这是泄漏路径
			// 将 OSS AccessKey + DB DSN 放入聊天回复中。
			var skillEnv map[string]string
			if envProvider != nil && skillDirs != nil {
				skillEnv = resolveSkillEnv(args.Command, envProvider, skillDirs)
			}
			sessEnv := buildSubprocessEnv(skillEnv)
			s, err := r.shellMgr.Start(command, sessEnv)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Started background shell %s for command: %s\nUse bash_output(bash_id=%q) to read output, kill_shell(bash_id=%q) to terminate.", s.id, args.Command, s.id, s.id), nil
		}

		if useSandbox && sbCfg != nil && sbCfg.Pool != nil {
			sb := sbCfg.Pool.Get(sbCfg.AgentID, sbCfg.Image, sbCfg.Workspace, sbCfg.Policy)
			out, err := sb.Exec(execCtx, command, "/workspace")
			return MetaSandboxPrefix + out, err
		}
		// 已请求沙箱，但没有连接执行器 - 拒绝
		// 比在主机外壳上运行。 SetExecutor 交换了这个闭包
		// 对于成功会话绑定的沙盒变体，所以我们
		// 仅当执行程序池失败时才登陆此处（docker daemon
		// 宕机、镜像拉取失败、容器启动错误）。返回一个
		// 清除错误使模型有机会将其浮现出来，而不是
		// 用户看到主机外壳“未找到命令”之谜。
		if useSandbox {
			return "", fmt.Errorf("sandbox required but no executor available — check that the sandbox backend (docker / e2b) is reachable and the configured image (%q) can start", sbCfgImage(sbCfg))
		}

		cmd := exec.CommandContext(execCtx, "sh", "-c", command)

		// 始终显式设置 cmd.Env。默认的 Go 行为是
		// 继承父级的完整环境，这会泄露守护进程的秘密
		// (BKCRAB_STORAGE_DSN, BKCRAB_OBJECT_STORE_*, ...) 到
		// 模型可以运行的每个 shell。
		var skillEnv map[string]string
		if envProvider != nil && skillDirs != nil {
			skillEnv = resolveSkillEnv(args.Command, envProvider, skillDirs)
		}
		cmd.Env = buildSubprocessEnv(skillEnv)

		output, err := cmd.CombinedOutput()

		result := string(output)
		if err != nil {
			return fmt.Sprintf("%s\nError: %s", result, err.Error()), err
		}

		return result, nil
	}
}

// sbCfgImage 返回诊断错误消息的沙箱映像名称。
// 返回“<unset>”，以便用户立即看到没有图像是均匀的
// 已配置（与已配置但无法访问）。
func sbCfgImage(sbCfg *SandboxConfig) string {
	if sbCfg == nil || sbCfg.Image == "" {
		return "<unset>"
	}
	return sbCfg.Image
}

// solveSkillEnv 检查命令路径是否引用技能目录
// 并返回技能的配置环境变量。
//
// 两条匹配路径：
// 1. SkillDirs 中的主机路径（例如“/Users/.../agents/<id>/skills”） —
// 当 exec 在主机 shell 上运行时使用。
// 2. 沙箱内部“/skills/<name>”前缀 — 每个技能都已安装
// 进入该位置的 docker 容器，无论它在哪里
// 存在于主机上，因此模型在内部写入命令
// 沙箱采用这种形式。没有这个分支，env注入
// 默默地破坏了所有沙箱调用（主机路径
// SkillDirs 永远不会出现在 /workspace-cd 命令中）。
func resolveSkillEnv(command string, envProvider SkillEnvProvider, skillDirs []string) map[string]string {
	// 1. 主机路径
	for _, dir := range skillDirs {
		if strings.Contains(command, dir) {
			rest := command[strings.Index(command, dir)+len(dir):]
			if len(rest) > 0 && rest[0] == '/' {
				rest = rest[1:]
			}
			parts := strings.SplitN(rest, "/", 2)
			if len(parts) > 0 && parts[0] != "" {
				if env := envProvider(parts[0]); env != nil {
					return env
				}
			}
		}
	}
	// 2. sandbox /skills/<name>/... — 固定安装布局
	if idx := strings.Index(command, "/skills/"); idx >= 0 {
		rest := command[idx+len("/skills/"):]
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) > 0 && parts[0] != "" {
			if env := envProvider(parts[0]); env != nil {
				return env
			}
		}
	}
	return nil
}

// mergeEnv 将基本环境与附加变量合并。附加变量覆盖基础。
func mergeEnv(base []string, additional map[string]string) []string {
	env := make([]string, 0, len(base)+len(additional))
	overridden := make(map[string]bool, len(additional))

	for _, e := range base {
		key := e
		if idx := strings.IndexByte(e, '='); idx >= 0 {
			key = e[:idx]
		}
		if _, ok := additional[key]; ok {
			overridden[key] = true
			continue // skip, will be added from additional
		}
		env = append(env, e)
	}

	for k, v := range additional {
		env = append(env, k+"="+v)
	}

	return env
}

// HostExecToolName 是为主机 shell 执行广告的工具名称
// 自托管安装。导出的调用者（loop.go Skill-dirs slice，
// 未来的审核日志等）可以参考它而无需重新串接
// 文字。
const HostExecToolName = "host_exec"

// registerHostExec 添加了一个 escape-hatch 执行工具，可以绕过
// 沙箱执行器并直接在操作员的主机外壳上运行。
// 由 buildinfo.IsHostExecAllowed() 控制 — 仅在以下情况下注册
// 操作员已通过 BKCRAB_ALLOW_HOST_EXEC=1 明确选择加入
// 并且存在沙箱执行器（否则“exec”已经是
// host shell 和 host_exec 将是重复的）。
//
// 工具描述大声阐明边界，以便模型选择
// 默认情况下`exec`（沙箱）并且仅转义到host_exec
// 真正的操作员环境工作（`bkcrab update`、`~/Downloads`、
// `launchctl`，系统服务，任何与用户实际相关的东西
// 机器）。危险命令候选名单仍然适用——沙箱 vs
// 主机不会更改“no rm -rf /”规则。
//
// 默认关闭的基本原理：在任何可通过
// 外部 IM 渠道（微信、Discord、飞书……），毫无戒心
// 将模型引入 host_exec 的喋喋不休是一种特权升级
// 小路。需要主机 shell 访问权限的操作员明确选择加入。
func registerHostExec(r *Registry, envProvider SkillEnvProvider, skillDirs []string) {
	r.Register(HostExecToolName,
		"Execute a shell command on the OPERATOR's host machine, bypassing the sandbox. "+
			"Use this ONLY for tasks tied to the user's actual environment — `bkcrab upgrade`, "+
			"reading their `~/Downloads`, listing host processes, running CLI tools they have "+
			"installed locally, similar host-side ops. For everything else (running scripts, "+
			"web requests, data processing, generating files for the user) use `exec` instead "+
			"so the work stays inside the sandbox. Same dangerous-command guard as exec; "+
			"`rm -rf /` and friends are still refused.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "The shell command to execute on the host.",
				},
				"stdin": map[string]interface{}{
					"type":        "string",
					"description": "Optional input piped to the command's stdin.",
				},
				"timeout": map[string]interface{}{
					"type":        "integer",
					"description": "Timeout in seconds (default 120). Headless-browser workflows (camoufox-cli) need a longer ceiling for the first call — the daemon + browser cold-start can take 2-3 min when traffic is proxied; subsequent calls in the same session are sub-second.",
				},
			},
			"required": []string{"command"},
		},
		func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
			var args execArgs
			if err := json.Unmarshal(rawArgs, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if args.Command == "" {
				return "", fmt.Errorf("command is required")
			}
			if err := rejectLearnerManagedCommand(args.Command); err != nil {
				return "", err
			}
			lower := strings.ToLower(args.Command)
			for _, dc := range dangerousCommands {
				if strings.Contains(lower, dc) {
					return "", fmt.Errorf("dangerous command blocked: %s", args.Command)
				}
			}
			timeout := 120
			if args.Timeout > 0 {
				timeout = args.Timeout
			}
			execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
			defer cancel()
			command := args.Command
			if args.Stdin != "" {
				command = fmt.Sprintf("(cat <<'__FCSTDIN__'\n%s\n__FCSTDIN__\n) | %s", args.Stdin, args.Command)
			}
			cmd := exec.CommandContext(execCtx, "sh", "-c", command)
			// host_exec 是操作员的逃生舱口——即便如此，擦洗
			// 来自继承的环境的守护进程秘密。操作员
			// 很少需要从主机访问 BKCRAB_STORAGE_DSN
			// shell，并且永远不需要模型能够读取它。
			var skillEnv map[string]string
			if envProvider != nil && skillDirs != nil {
				skillEnv = resolveSkillEnv(args.Command, envProvider, skillDirs)
			}
			cmd.Env = buildSubprocessEnv(skillEnv)
			out, err := cmd.CombinedOutput()
			result := string(out)
			if err != nil {
				return fmt.Sprintf("%s\nError: %s", result, err.Error()), err
			}
			return result, nil
		})
}

// registerSandboxedExec 重新注册 exec 工具，以便它委托给
// sandbox.Executor而不是在主机上运行。技能环境变量
// 通过管理 UI 配置的（FAL_KEY、REPLICATE_API_TOKEN 等）是
// 通过预先添加 POSIX `export` 语句注入到容器中
// 命令 - sandbox.Executor.Exec 只接受单个命令
// 字符串，因此我们无法像主机那样通过进程属性传递 env
// 路径确实如此。
func registerSandboxedExec(r *Registry, ex sandbox.Executor) {
	envProvider := r.envProvider
	skillDirs := r.skillDirs
	r.Register("exec", "Execute a shell command in the sandbox and return stdout/stderr. For binary or image output (PNG, JPEG, PDF, audio, video), write the file into the workspace (e.g. ./out.png) and reference it by relative path in your reply — do NOT base64-encode it into stdout, and do NOT inline data: URLs in your response. The workspace file will be surfaced to the user via the Files panel.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"stdin": map[string]interface{}{
				"type":        "string",
				"description": "Optional input piped to the command's stdin.",
			},
			"timeout": map[string]interface{}{
				"type":        "integer",
				"description": "Timeout in seconds (default 120). Headless-browser workflows (camoufox-cli) need a longer ceiling for the first call — the daemon + browser cold-start can take 2-3 min when traffic is proxied; subsequent calls in the same session are sub-second.",
			},
			"run_in_background": map[string]interface{}{
				"type":        "boolean",
				"description": "Launch in background. NOT YET SUPPORTED in sandbox mode — use `tmux new-session -d -s NAME '<cmd>'` directly instead.",
			},
		},
		"required": []string{"command"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args execArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Command == "" {
			return "", fmt.Errorf("command is required")
		}
		if err := rejectLearnerManagedCommand(args.Command); err != nil {
			return "", err
		}
		if args.RunInBackground {
			return "", fmt.Errorf("run_in_background is not yet supported in sandbox mode — use tmux inside the sandbox instead: exec({command: \"tmux new-session -d -s job '<your command>'\"}) to start, exec({command: \"tmux capture-pane -t job -p\"}) to read, exec({command: \"tmux kill-session -t job\"}) to stop")
		}
		timeout := 120
		if args.Timeout > 0 {
			timeout = args.Timeout
		}
		command := args.Command
		// 通过heredoc（镜像主机路径）的标准输入，以便调用者可以进行管道传输
		// 技能脚本的 JSON 参数。
		if args.Stdin != "" {
			command = fmt.Sprintf("(cat <<'__FCSTDIN__'\n%s\n__FCSTDIN__\n) | %s", args.Stdin, args.Command)
		}
		// 为命令中的任意技能注入配置的环境
		// 参考（SK 技能目录可以是主机路径或
		// 容器内部 /skills/<name> 挂载 —resolveSkillEnv
		// 两者都匹配）。
		injected := []string{}
		if envProvider != nil {
			skillEnv := resolveSkillEnv(args.Command, envProvider, skillDirs)
			if len(skillEnv) > 0 {
				var sb strings.Builder
				for k, v := range skillEnv {
					sb.WriteString("export ")
					sb.WriteString(k)
					sb.WriteString("=")
					sb.WriteString(shellQuote(v))
					sb.WriteString("; ")
					if v == "" {
						injected = append(injected, k+"=<empty>")
					} else {
						injected = append(injected, k+"=<set "+strconv.Itoa(len(v))+"chars>")
					}
				}
				sb.WriteString(command)
				command = sb.String()
			}
		}
		slog.Info("sandboxed exec",
			"backend", ex.Backend(),
			"envProviderSet", envProvider != nil,
			"skillDirsCount", len(skillDirs),
			"injected", injected,
			"cmdHead", firstN(args.Command, 80))
		out, err := ex.Exec(ctx, command, time.Duration(timeout)*time.Second)
		// 提示，不要自动回退：自动重试主机 shell 会
		// 在任何提示注入上悄悄地突破沙箱边界
		// “使其在沙箱中失败”技巧并会重新运行可能的
		// 不同文件系统中的命令错误。表面提示
		// 相反，LLM（或其经过操作员训练的 ChatBot）使得
		// 明确的决定。仅当 host_exec 为时附加提示
		// 实际上可用 - 在托管部署上它不可用，并且
		// 建议一个不存在的工具只会让人们感到困惑
		// 模型。我们通过工具名称进行探测，以便检查与
		// 部署模式标志 - 相同的答案，更少的耦合。
		if err != nil && looksLikeSandboxAbsence(err, out) && buildinfo.IsHostExecAllowed() {
			err = fmt.Errorf("%w\n[hint: this looks like a sandbox-environment miss (binary or path not present in the container). If the command needs the user's actual host machine — e.g. `bkcrab upgrade`, `~/Downloads`, host CLI tools — retry with the `host_exec` tool instead.]", err)
		}
		return MetaSandboxPrefix + out, err
	})
}

// looksLikeSandboxAbsence 嗅探常见的执行错误/输出
// “尝试在沙箱内运行仅主机的东西”签名，因此
// 我们附加到错误的提示是有针对性的，而不是嘈杂的。保守的 -
// 返回 false 除非我们相当确定：真正的失败（例如脚本
// 运行中崩溃）不应得到“使用 host_exec 代替”建议
// 那只会让法学硕士走上错误的道路。
func looksLikeSandboxAbsence(err error, out string) bool {
	if err == nil {
		return false
	}
	hay := strings.ToLower(err.Error() + "\n" + out)
	patterns := []string{
		"command not found",
		"not found in path",
		": no such file or directory",
		"executable file not found",
	}
	for _, p := range patterns {
		if strings.Contains(hay, p) {
			return true
		}
	}
	return false
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// shellQuote 单引号将值转义为安全插值
// POSIX shell 命令。由沙盒执行程序用来前置环境变量
// 而不将未转义的值暴露给 shell 元字符。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
