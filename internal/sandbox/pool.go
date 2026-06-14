package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SandboxPool 按代理管理沙箱容器，跨 exec 调用重用它们。
type SandboxPool struct {
	sandboxes map[string]*DockerSandbox // agentID -> 沙箱
	mu        sync.Mutex
}

// NewPool 创建一个新的沙箱池。
func NewPool() *SandboxPool {
	return &SandboxPool{
		sandboxes: make(map[string]*DockerSandbox),
	}
}

// Get 返回（或延迟创建）给定代理的沙箱。
//
// 创建时，我们将两个技能目录都连接到沙箱中，
// 以便 LLM 的 `python /skills/<name>/main.py` 可以解析技能是位于
// 全局 $BKCLAW_HOME/skills/ 树中还是此代理的私有
// $BKCLAW_HOME/agents/<agentID>/agent/skills/ 中。
// 没有按代理挂载，操作员放入 agents/<id>/agent/skills/ 的技能
//（例如通过 SkillsLoader 的按代理层）会在容器内静默地加载失败。
func (p *SandboxPool) Get(agentID, image, workspace string, policy *Policy) *DockerSandbox {
	p.mu.Lock()
	defer p.mu.Unlock()

	if sb, ok := p.sandboxes[agentID]; ok {
		return sb
	}

	sb := NewDockerSandbox(image, workspace, policy)
	// 当工作区路径遵循标准布局（<home>/workspaces/<agentID>）时
	// 尽力尝试技能挂载；如果调用者使用了自定义工作区路径则静默跳过。
	if home := homeFromWorkspace(workspace, agentID); home != "" {
		if dirs := skillDirsForAgent(home, agentID); len(dirs) > 0 {
			sb.SetSkillDirs(dirs)
		}
	}
	p.sandboxes[agentID] = sb
	return sb
}

// homeFromWorkspace 将 <home>/workspaces/<agentID> 反解为 <home>。
// 如果工作区不遵循该约定则返回 ""。
func homeFromWorkspace(workspace, agentID string) string {
	suffix := filepath.Join("workspaces", agentID)
	if strings.HasSuffix(workspace, string(os.PathSeparator)+suffix) {
		return strings.TrimSuffix(workspace, string(os.PathSeparator)+suffix)
	}
	return ""
}

// skillDirsForAgent 返回主机路径，其 `<dir>/<skill-name>/` 子目录
// 应挂载到沙箱内的 /skills/<skill-name>/ 下。
// 按代理目录优先，以便其技能覆盖同名的全局技能，匹配 SkillsLoader 优先级。
//
// home 是解析后的 BKCLAW_HOME（池的 workspaceRoot），
// 而不是进程环境——保持测试/多实例调试的真实性。
func skillDirsForAgent(home, agentID string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, "agents", agentID, "agent", "skills"),
		filepath.Join(home, "skills"),
	}
}

// Close 关闭并移除所有沙箱容器。
func (p *SandboxPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for id, sb := range p.sandboxes {
		sb.Close()
		delete(p.sandboxes, id)
	}
}

// List 返回所有活动沙箱的信息。
func (p *SandboxPool) List() []SandboxInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	var infos []SandboxInfo
	for agentID, sb := range p.sandboxes {
		infos = append(infos, SandboxInfo{
			AgentID:     agentID,
			ContainerID: sb.ContainerID(),
			Image:       sb.image,
			Workspace:   sb.workspace,
		})
	}
	return infos
}

// Remove 按代理 ID 销毁特定的沙箱。
func (p *SandboxPool) Remove(agentID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sb, ok := p.sandboxes[agentID]
	if !ok {
		return nil
	}
	err := sb.Close()
	delete(p.sandboxes, agentID)
	return err
}

// SandboxInfo 保存沙箱的显示信息。
type SandboxInfo struct {
	AgentID     string
	ContainerID string
	Image       string
	Workspace   string
}
