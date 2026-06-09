package policy

// Policy defines what an agent is allowed to do.
type Policy struct {
	Name        string      `yaml:"name"        json:"name"`
	Description string      `yaml:"description,omitempty" json:"description,omitempty"`
	Filesystem  FSPolicy    `yaml:"filesystem,omitempty"  json:"filesystem,omitempty"`
	Network     NetPolicy   `yaml:"network,omitempty"     json:"network,omitempty"`
	Tools       ToolsPolicy `yaml:"tools,omitempty"       json:"tools,omitempty"`
	Resources   ResPolicy   `yaml:"resources,omitempty"   json:"resources,omitempty"`
}

// FSPolicy controls filesystem access.
type FSPolicy struct {
	AllowRead  []string `yaml:"allowRead,omitempty"  json:"allowRead,omitempty"`
	AllowWrite []string `yaml:"allowWrite,omitempty" json:"allowWrite,omitempty"`
	DenyRead   []string `yaml:"denyRead,omitempty"   json:"denyRead,omitempty"`
	DenyWrite  []string `yaml:"denyWrite,omitempty"  json:"denyWrite,omitempty"`
}

// NetPolicy controls network access.
type NetPolicy struct {
	Outbound []NetRule `yaml:"outbound,omitempty" json:"outbound,omitempty"`
	Mode     string   `yaml:"mode,omitempty"     json:"mode,omitempty"` // "none", "allowlist", "permissive"
}

// NetRule defines an outbound network allowlist entry.
type NetRule struct {
	Host    string   `yaml:"host"              json:"host"`
	Ports   []int    `yaml:"ports,omitempty"   json:"ports,omitempty"`
	Methods []string `yaml:"methods,omitempty" json:"methods,omitempty"`
	Paths   []string `yaml:"paths,omitempty"   json:"paths,omitempty"`
}

// ToolsPolicy controls which tools are available.
type ToolsPolicy struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"` // tool names, * = all
	Deny  []string `yaml:"deny,omitempty"  json:"deny,omitempty"` // deny wins over allow
}

// ResPolicy controls resource limits for sandbox.
type ResPolicy struct {
	MaxCPU      string `yaml:"maxCpu,omitempty"      json:"maxCpu,omitempty"`
	MaxMemory   string `yaml:"maxMemory,omitempty"   json:"maxMemory,omitempty"`
	MaxDiskMB   int    `yaml:"maxDiskMb,omitempty"   json:"maxDiskMb,omitempty"`
	ExecTimeout int    `yaml:"execTimeoutSec,omitempty" json:"execTimeoutSec,omitempty"`
}
