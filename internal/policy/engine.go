package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Engine 评估策略规则。
type Engine struct {
	policy *Policy
}

// NewEngine 使用给定的策略创建一个引擎。
func NewEngine(p *Policy) *Engine {
	if p == nil {
		p = DefaultPolicy()
	}
	return &Engine{policy: p}
}

// Policy 返回当前策略。
func (e *Engine) Policy() *Policy {
	return e.policy
}

// LoadFromFile 解析 YAML 策略文件。
func LoadFromFile(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse policy YAML: %w", err)
	}
	return &p, nil
}

// LoadPreset 返回指定名称的预设策略。
func LoadPreset(name string) *Policy {
	switch strings.ToLower(name) {
	case "restricted":
		return RestrictedPolicy()
	case "standard":
		return StandardPolicy()
	default:
		return DefaultPolicy()
	}
}

// CheckFilesystem 检查文件路径是否允许读取或写入。
func (e *Engine) CheckFilesystem(path string, write bool) error {
	fs := e.policy.Filesystem

	if write {
		// 先检查拒绝规则（拒绝优先）
		for _, pattern := range fs.DenyWrite {
			if matchGlob(pattern, path) {
				return fmt.Errorf("policy: write denied for %s (matches %s)", path, pattern)
			}
		}
		// 如果指定了允许列表，路径必须匹配
		if len(fs.AllowWrite) > 0 {
			if !matchAny(fs.AllowWrite, path) {
				return fmt.Errorf("policy: write not allowed for %s", path)
			}
		}
	} else {
		// 先检查拒绝规则
		for _, pattern := range fs.DenyRead {
			if matchGlob(pattern, path) {
				return fmt.Errorf("policy: read denied for %s (matches %s)", path, pattern)
			}
		}
		if len(fs.AllowRead) > 0 {
			if !matchAny(fs.AllowRead, path) {
				return fmt.Errorf("policy: read not allowed for %s", path)
			}
		}
	}
	return nil
}

// CheckNetwork 检查网络请求是否允许。
func (e *Engine) CheckNetwork(host string, port int, method string, path string) error {
	net := e.policy.Network

	switch net.Mode {
	case "none":
		return fmt.Errorf("policy: all network access denied")
	case "permissive", "":
		return nil
	case "allowlist":
		// 必须匹配至少一条出站规则
		for _, rule := range net.Outbound {
			if !matchHost(rule.Host, host) {
				continue
			}
			if len(rule.Ports) > 0 && !containsInt(rule.Ports, port) {
				continue
			}
			if len(rule.Methods) > 0 && !containsStr(rule.Methods, strings.ToUpper(method)) {
				continue
			}
			if len(rule.Paths) > 0 && !matchAny(rule.Paths, path) {
				continue
			}
			return nil // matched
		}
		return fmt.Errorf("policy: network access denied for %s:%d", host, port)
	}
	return nil
}

// CheckTool 检查工具是否允许使用。
func (e *Engine) CheckTool(toolName string) error {
	tools := e.policy.Tools

	// 拒绝规则始终优先
	for _, d := range tools.Deny {
		if d == toolName || d == "*" {
			return fmt.Errorf("policy: tool %q denied", toolName)
		}
	}

	// 如果指定了允许列表，必须在列表中
	if len(tools.Allow) > 0 {
		for _, a := range tools.Allow {
			if a == toolName || a == "*" {
				return nil
			}
		}
		return fmt.Errorf("policy: tool %q not allowed", toolName)
	}

	return nil
}

func matchGlob(pattern, path string) bool {
	matched, _ := filepath.Match(pattern, path)
	if matched {
		return true
	}
	// 也尝试匹配基本名称
	matched, _ = filepath.Match(pattern, filepath.Base(path))
	return matched
}

func matchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if matchGlob(p, path) {
			return true
		}
		// 支持带尾随 * 的前缀匹配
		if strings.HasSuffix(p, "*") && strings.HasPrefix(path, strings.TrimSuffix(p, "*")) {
			return true
		}
		// 支持目录前缀（例如 "/workspace" 允许 "/workspace/foo"）
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func matchHost(pattern, host string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		// 通配符子域名匹配
		suffix := pattern[1:] // 如 ".example.com"
		return strings.HasSuffix(host, suffix) || host == pattern[2:]
	}
	return pattern == host
}

func containsInt(slice []int, val int) bool {
	for _, v := range slice {
		if v == val {
			return true
		}
	}
	return false
}

func containsStr(slice []string, val string) bool {
	for _, v := range slice {
		if strings.EqualFold(v, val) {
			return true
		}
	}
	return false
}
