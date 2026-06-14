package tools

// bash_output 和kill_shell — exec(run_in_background) 的配套工具。
//
// 工具表面镜像 Claude Code 的 `BashOutput` / `KillShell` 所以提示
// 以及为该运行时移植编写的技能，无需翻译。

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type bashOutputArgs struct {
	BashID string `json:"bash_id"`
	Filter string `json:"filter,omitempty"` // optional regex applied per output line
}

type killShellArgs struct {
	BashID string `json:"bash_id"`
}

const bashOutputDescription = `Read new stdout/stderr from a backgrounded shell since the last call. Use this to monitor a long-running process started with exec(run_in_background=true).

Returns:
  - new output produced since the previous bash_output call on this bash_id (each call advances a per-session cursor)
  - "[status] running" or "[status] exited (code=N)" — only "exited" rows guarantee the process is done; killed processes report code=-1 with the kill reason appended
  - a "[truncated]" line prepended if the 4 MiB per-session output buffer rolled past the read cursor (oldest bytes dropped FIFO)

Notes:
  - The session keeps running across calls until kill_shell or natural exit.
  - After exit, bash_output is still callable to read any final output and confirm the exit code.
  - The optional 'filter' regex is applied per output line (lines that don't match are dropped before return) — useful for tailing a noisy log when you only care about errors.`

const killShellDescription = `Terminate a backgrounded shell started by exec(run_in_background=true). Sends SIGKILL via process-group cancellation. Idempotent — calling it on an already-exited shell is a no-op and returns success.`

func registerBashOutput(r *Registry) {
	r.Register("bash_output", bashOutputDescription, map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bash_id": map[string]interface{}{
				"type":        "string",
				"description": "Identifier returned by exec(run_in_background=true), e.g. \"bash_3\".",
			},
			"filter": map[string]interface{}{
				"type":        "string",
				"description": "Optional regex (RE2). Only output lines matching this pattern are returned.",
			},
		},
		"required": []string{"bash_id"},
	}, func(ctx context.Context, raw json.RawMessage) (string, error) {
		var args bashOutputArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("bash_output: parse args: %w", err)
		}
		if args.BashID == "" {
			return "", fmt.Errorf("bash_output: bash_id is required")
		}
		if r.shellMgr == nil {
			return "", fmt.Errorf("bash_output: shell manager not initialised")
		}
		s := r.shellMgr.Get(args.BashID)
		if s == nil {
			return "", fmt.Errorf("bash_output: no such bash_id %q (call exec(run_in_background=true) first; ids are valid only within the same agent process)", args.BashID)
		}

		var filter *regexp.Regexp
		if args.Filter != "" {
			re, err := regexp.Compile(args.Filter)
			if err != nil {
				return "", fmt.Errorf("bash_output: invalid filter regex: %w", err)
			}
			filter = re
		}

		raw2, dropped := s.readNew()
		status, code, exitErr := s.snapshot()
		// 竞争修复：字节可以在 readNew 之后进入缓冲区，但是
		// 在收割者翻转之前，done=true。如果没有这个排水管，
		// “退出”报告会默默地错过最后几个字节（通常
		// 最有用的——错误消息或摘要行）。
		// 仅在退出时排水；正在运行的 shell 可以稍后再次轮询
		// 以获得新的输出。
		if status == statusExited {
			more, dropped2 := s.readNew()
			if len(more) > 0 {
				raw2 = append(raw2, more...)
			}
			dropped = dropped || dropped2
		}
		body := string(raw2)
		if filter != nil && body != "" {
			body = filterLines(body, filter)
		}

		var sb strings.Builder
		if dropped {
			sb.WriteString("[truncated] earlier output exceeded the 4 MiB session cap and was dropped\n")
		}
		if body != "" {
			sb.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				sb.WriteByte('\n')
			}
		}
		switch status {
		case statusRunning:
			sb.WriteString("[status] running")
		case statusExited:
			fmt.Fprintf(&sb, "[status] exited (code=%d)", code)
			if exitErr != nil && code == -1 {
				// 异常：killed、IO错误等。
				fmt.Fprintf(&sb, " — %s", exitErr.Error())
			}
		}
		return sb.String(), nil
	})
}

func registerKillShell(r *Registry) {
	r.Register("kill_shell", killShellDescription, map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bash_id": map[string]interface{}{
				"type":        "string",
				"description": "Identifier returned by exec(run_in_background=true).",
			},
		},
		"required": []string{"bash_id"},
	}, func(ctx context.Context, raw json.RawMessage) (string, error) {
		var args killShellArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("kill_shell: parse args: %w", err)
		}
		if args.BashID == "" {
			return "", fmt.Errorf("kill_shell: bash_id is required")
		}
		if r.shellMgr == nil {
			return "", fmt.Errorf("kill_shell: shell manager not initialised")
		}
		s := r.shellMgr.Get(args.BashID)
		if s == nil {
			return "", fmt.Errorf("kill_shell: no such bash_id %q", args.BashID)
		}
		if s.done.Load() {
			_, code, _ := s.snapshot()
			return fmt.Sprintf("Already exited (code=%d).", code), nil
		}
		_ = s.kill()
		return fmt.Sprintf("Sent kill to %s.", s.id), nil
	})
}

// filterLines 仅保留与 re 匹配的行。尾随换行策略
// 跟随输入：带有尾随换行符的正文保留它，一个
// 没有则不然。不匹配的行将被默默删除。
func filterLines(body string, re *regexp.Regexp) string {
	hadTrailingNL := strings.HasSuffix(body, "\n")
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if re.MatchString(l) {
			kept = append(kept, l)
		}
	}
	out := strings.Join(kept, "\n")
	if hadTrailingNL && out != "" {
		out += "\n"
	}
	return out
}
