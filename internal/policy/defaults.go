package policy

// DefaultPolicy returns a permissive policy that allows everything.
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

// RestrictedPolicy returns a locked-down policy that denies everything by default.
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

// StandardPolicy returns sensible defaults.
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
