package policy

// DefaultPolicy 返回允许所有操作的宽松策略。
func DefaultPolicy() *Policy {
	return &Policy{
		Name:        "permissive",
		Description: "Allows all operations (default)",
		Network: NetPolicy{
			Mode: "permissive",
		},
		Tools: ToolsPolicy{
			Allow: []string{"*"},
		},
	}
}

// RestrictedPolicy 返回默认拒绝所有操作的严格限制策略。
func RestrictedPolicy() *Policy {
	return &Policy{
		Name:        "restricted",
		Description: "Denies all operations unless explicitly allowed",
		Filesystem: FSPolicy{
			DenyWrite: []string{"/etc/*", "/usr/*", "/bin/*", "/sbin/*", "/var/*"},
			DenyRead:  []string{"/etc/shadow", "/etc/passwd"},
		},
		Network: NetPolicy{
			Mode: "none",
		},
		Tools: ToolsPolicy{
			Deny: []string{"exec"},
		},
		Resources: ResPolicy{
			MaxCPU:      "1",
			MaxMemory:   "256m",
			ExecTimeout: 30,
		},
	}
}

// StandardPolicy 返回合理的默认策略。
func StandardPolicy() *Policy {
	return &Policy{
		Name:        "standard",
		Description: "Sensible defaults: no write to system dirs, allowlist network",
		Filesystem: FSPolicy{
			DenyWrite: []string{"/etc/*", "/usr/*", "/bin/*", "/sbin/*"},
			DenyRead:  []string{"/etc/shadow"},
		},
		Network: NetPolicy{
			Mode: "permissive",
		},
		Tools: ToolsPolicy{
			Allow: []string{"*"},
		},
		Resources: ResPolicy{
			MaxCPU:      "2",
			MaxMemory:   "512m",
			ExecTimeout: 60,
		},
	}
}
