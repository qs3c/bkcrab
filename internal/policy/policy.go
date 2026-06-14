package policy

// Policy 定义了代理允许执行的操作。
type Policy struct {
	Name        string      `yaml:"name"        json:"name"`
	Description string      `yaml:"description,omitempty" json:"description,omitempty"`
	Filesystem  FSPolicy    `yaml:"filesystem,omitempty"  json:"filesystem,omitempty"`
	Network     NetPolicy   `yaml:"network,omitempty"     json:"network,omitempty"`
	Tools       ToolsPolicy `yaml:"tools,omitempty"       json:"tools,omitempty"`
	Resources   ResPolicy   `yaml:"resources,omitempty"   json:"resources,omitempty"`
}

// FSPolicy 控制文件系统访问。
type FSPolicy struct {
	AllowRead  []string `yaml:"allowRead,omitempty"  json:"allowRead,omitempty"`
	AllowWrite []string `yaml:"allowWrite,omitempty" json:"allowWrite,omitempty"`
	DenyRead   []string `yaml:"denyRead,omitempty"   json:"denyRead,omitempty"`
	DenyWrite  []string `yaml:"denyWrite,omitempty"  json:"denyWrite,omitempty"`
}

// NetPolicy 控制网络访问。
type NetPolicy struct {
	Outbound []NetRule `yaml:"outbound,omitempty" json:"outbound,omitempty"`
	Mode     string   `yaml:"mode,omitempty"     json:"mode,omitempty"` // 可选值："none"、"allowlist"、"permissive"
}

// NetRule 定义出站网络允许列表条目。
type NetRule struct {
	Host    string   `yaml:"host"              json:"host"`
	Ports   []int    `yaml:"ports,omitempty"   json:"ports,omitempty"`
	Methods []string `yaml:"methods,omitempty" json:"methods,omitempty"`
	Paths   []string `yaml:"paths,omitempty"   json:"paths,omitempty"`
}

// ToolsPolicy 控制哪些工具可用。
type ToolsPolicy struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"` // 工具名称，* = 全部
	Deny  []string `yaml:"deny,omitempty"  json:"deny,omitempty"` // 拒绝规则优先于允许规则
}

// ResPolicy 控制沙箱的资源限制。
type ResPolicy struct {
	MaxCPU      string `yaml:"maxCpu,omitempty"      json:"maxCpu,omitempty"`
	MaxMemory   string `yaml:"maxMemory,omitempty"   json:"maxMemory,omitempty"`
	MaxDiskMB   int    `yaml:"maxDiskMb,omitempty"   json:"maxDiskMb,omitempty"`
	ExecTimeout int    `yaml:"execTimeoutSec,omitempty" json:"execTimeoutSec,omitempty"`
}
