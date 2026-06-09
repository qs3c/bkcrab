package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Engine evaluates policy rules.
type Engine struct {
	policy *Policy
}

// NewEngine creates an engine with the given policy.
func NewEngine(p *Policy) *Engine {
	if p == nil {
		p = DefaultPolicy()
	}
	return &Engine{policy: p}
}

// Policy returns the current policy.
func (e *Engine) Policy() *Policy {
	return e.policy
}

// LoadFromFile parses a YAML policy file.
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

// LoadPreset returns a named preset policy.
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

// CheckFilesystem checks whether a file path is allowed for read or write.
func (e *Engine) CheckFilesystem(path string, write bool) error {
	fs := e.policy.Filesystem

	if write {
		// Check deny first (deny wins)
		for _, pattern := range fs.DenyWrite {
			if matchGlob(pattern, path) {
				return fmt.Errorf("policy: write denied for %s (matches %s)", path, pattern)
			}
		}
		// If allow list is specified, path must match
		if len(fs.AllowWrite) > 0 {
			if !matchAny(fs.AllowWrite, path) {
				return fmt.Errorf("policy: write not allowed for %s", path)
			}
		}
	} else {
		// Check deny first
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

// CheckNetwork checks whether a network request is allowed.
func (e *Engine) CheckNetwork(host string, port int, method string, path string) error {
	net := e.policy.Network

	switch net.Mode {
	case "none":
		return fmt.Errorf("policy: all network access denied")
	case "permissive", "":
		return nil
	case "allowlist":
		// Must match at least one outbound rule
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

// CheckTool checks whether a tool is allowed to be used.
func (e *Engine) CheckTool(toolName string) error {
	tools := e.policy.Tools

	// Deny always wins
	for _, d := range tools.Deny {
		if d == toolName || d == "*" {
			return fmt.Errorf("policy: tool %q denied", toolName)
		}
	}

	// If allow list is specified, must be in it
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
	// Also try matching against the base name
	matched, _ = filepath.Match(pattern, filepath.Base(path))
	return matched
}

func matchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if matchGlob(p, path) {
			return true
		}
		// Support prefix matching with trailing *
		if strings.HasSuffix(p, "*") && strings.HasPrefix(path, strings.TrimSuffix(p, "*")) {
			return true
		}
		// Support directory prefix (e.g. "/workspace" allows "/workspace/foo")
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
		// Wildcard subdomain match
		suffix := pattern[1:] // ".example.com"
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
