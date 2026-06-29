package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Policy 保存沙箱容器的资源/网络约束。
type Policy struct {
	MaxCPU    string // 例如 "2"
	MaxMemory string // 例如 "512m"
	NetMode   string // "none"、"host"、"bridge"
}

// DockerSandbox 管理单个 Docker 容器以进行沙箱化执行。
type DockerSandbox struct {
	containerID string
	image       string
	workspace   string
	// workdir 是容器的起始工作目录。空时默认为 /workspace。
	// 项目聊天覆盖为 /workspace/<sessionID>/，以便聊天在 /workspace
	// 中看到整个项目（包括兄弟会话），但其相对写入默认为自己的子目录。
	workdir   string
	skillDirs []string // 主机路径，以只读方式挂载到 /skills/<name>/
	// userSkillsHostDir 当非空时，在容器内的 /root/.agents/skills
	// 处以读写方式绑定挂载。这是 `npx skills add -g -y`
	//（find-skills 告诉代理安装社区技能的方式）写入其全局安装的位置——
	// 因此聊天期间安装的任何技能直接进入聊天者的按用户主机目录，
	// 并在沙箱驱逐后持久存在。空值表示不挂载，
	// 这对于不携带聊天者身份的遗留/系统注入调用是正确的行为。
	userSkillsHostDir string
	policy            *Policy
	env               map[string]string
	mu                sync.Mutex
}

// NewDockerSandbox 创建一个新的沙箱配置（容器延迟创建）。
//
// 默认策略让 NetMode 保持未设置状态，这意味着 Docker 使用默认桥接网络
// （= 互联网访问）。大多数产品代理需要出站 HTTP 用于 LLM 提供者调用/
// 图片 API/pip 安装；过去以 NetMode="none" 锁定沙箱是默认行为，
// 并静默地破坏了生成图像类技能，导致 DNS 解析错误。
// 想要硬隔离的操作员传递具有 NetMode: "none" 的显式策略。
func NewDockerSandbox(image, workspace string, policy *Policy) *DockerSandbox {
	if image == "" {
		image = "thinkany/bkcrab-sandbox:latest"
	}
	if policy == nil {
		policy = &Policy{}
	}
	return &DockerSandbox{
		image:     image,
		workspace: workspace,
		policy:    policy,
		env:       make(map[string]string),
	}
}

// SetWorkdir 覆盖容器的起始工作目录。
// 空值恢复默认值（/workspace）。必须在 Create() 之前调用——
// 一旦容器存在，workdir 就已经固定了。
func (s *DockerSandbox) SetWorkdir(wd string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workdir = wd
}

// SetSkillDirs 配置主机路径，其内容（技能文件夹）应在沙箱内的
// /skills/<skill-name>/ 下可见。LLM 被告知通过
// `python /skills/<name>/main.py` 调用技能，因此没有这些挂载，
// 脚本文件在容器中不存在。传递的路径以只读方式挂载。
func (s *DockerSandbox) SetSkillDirs(dirs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skillDirs = append(s.skillDirs[:0], dirs...)
}

// SetUserSkillsHostDir 告诉 Create() 将此主机目录以读写方式绑定挂载到
// 沙箱内的 /root/.agents/skills——`npx skills add -g -y` 写入的位置。
// 空值禁用挂载（无按用户持久化）。调用者负责确保目录存在；
// Create() 会防御性地 mkdir，但权限错误会静默降级为"不挂载"，
// 而不是使沙箱启动失败。
func (s *DockerSandbox) SetUserSkillsHostDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userSkillsHostDir = dir
}

// SetEnv 设置要注入到容器中的环境变量。
func (s *DockerSandbox) SetEnv(env map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range env {
		s.env[k] = v
	}
}

// Create 创建 Docker 容器。
func (s *DockerSandbox) Create() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.containerID != "" {
		return nil // already created
	}

	args := []string{
		"create",
		"--interactive",
		"--label", "bkcrab=sandbox",
	}

	// 继承主机的 HTTP(S)_PROXY 配置，以便沙箱内的 curl/pip/npm/git
	// 可以通过 bkcrab 自身使用的任何代理访问被屏蔽的来源。
	// 没有这个，在受限网络（GFW 等）中，目标域名的 DNS 解析到黑洞，
	// 容器会看到 TLS 重置，在 Camoufox/Playwright 中表现为
	// NS_ERROR_NET_INTERRUPT。绑定到 localhost 的代理 URL 被重写为
	// host.docker.internal——容器内的 `127.0.0.1` 指的是容器本身，
	// 而不是主机。云部署通常没有代理环境变量，因此此循环在那里是空操作。
	rewroteToHostInternal := false
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		// 只重写代理 URL 本身，不重写 NO_PROXY 的绕过列表。
		switch k {
		case "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy":
			if strings.Contains(v, "127.0.0.1") || strings.Contains(v, "localhost") {
				v = strings.ReplaceAll(v, "127.0.0.1", "host.docker.internal")
				v = strings.ReplaceAll(v, "localhost", "host.docker.internal")
				rewroteToHostInternal = true
			}
		}
		// 保留操作员提供的环境变量（SetEnv）——显式覆盖继承的。
		if _, set := s.env[k]; !set {
			args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
		}
	}

	// 仅当我们实际重写了代理 URL 指向 host.docker.internal 时才强制解析它。
	// 始终添加此标志在现代 Docker（20.10+ host-gateway 关键字）上是无害的，
	// 但有条件地添加它可以使云路径——未设置代理环境变量——
	// 与更改前的行为保持字节相同，并避免重复 Docker Desktop/OrbStack
	// 已经自行注入的 host.docker.internal 条目。
	if rewroteToHostInternal {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}

	// Mount workspace
	if s.workspace != "" {
		args = append(args, "-v", fmt.Sprintf("%s:/workspace:rw", s.workspace))
		wd := s.workdir
		if wd == "" {
			wd = "/workspace"
		}
		args = append(args, "-w", wd)
	}

	// 将每个技能目录以只读方式挂载到 /skills/<basename>/。
	// LLM 被告知通过 `python /skills/<name>/main.py` 调用技能，
	// 因此没有这些挂载，脚本文件在容器中不存在。
	// 当没有显式设置目录时，自动默认使用 BKCRAB_HOME/skills/，
	// 以便新安装的产品代理无需操作员自己配置 SetSkillDirs 即可工作。
	dirs := s.skillDirs
	if len(dirs) == 0 {
		if h := os.Getenv("BKCRAB_HOME"); h != "" {
			dirs = []string{filepath.Join(h, "skills")}
		} else if home, err := os.UserHomeDir(); err == nil {
			dirs = []string{filepath.Join(home, ".bkcrab", "skills")}
		}
	}
	mounted := make(map[string]bool)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			// 按容器路径去重。较早的目录优先（按代理先于全局），
			// 匹配 SkillsLoader 的优先级。
			if mounted[e.Name()] {
				continue
			}
			mounted[e.Name()] = true
			host := filepath.Join(dir, e.Name())
			args = append(args, "-v", fmt.Sprintf("%s:/skills/%s:ro", host, e.Name()))
		}
	}

	// `npx skills add -g -y` 的按用户读写挂载——CLI 将其全局安装写入
	// 沙箱内的 /root/.agents/skills/<name>/，因此将该路径绑定到
	// 聊天者的按用户主机目录意味着安装持久化在主机磁盘上，
	// SkillsLoader 在下一轮拾取它们，无需任何额外管道。
	// mkdir 是防御性的；失败时静默处理，因此损坏的主机目录降级为
	// "代理在沙箱内安装失败"而不是"沙箱拒绝启动"。
	if s.userSkillsHostDir != "" {
		if err := os.MkdirAll(s.userSkillsHostDir, 0o755); err == nil {
			args = append(args, "-v", fmt.Sprintf("%s:/root/.agents/skills:rw", s.userSkillsHostDir))
		}
	}

	// Resource limits
	if s.policy.MaxCPU != "" {
		args = append(args, "--cpus", s.policy.MaxCPU)
	}
	if s.policy.MaxMemory != "" {
		args = append(args, "--memory", s.policy.MaxMemory)
	}

	// Network mode
	if s.policy.NetMode != "" {
		args = append(args, "--network", s.policy.NetMode)
	}

	// Environment variables
	for k, v := range s.env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	args = append(args, s.image, "tail", "-f", "/dev/null")

	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker create: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	s.containerID = strings.TrimSpace(stdout.String())

	// Start the container
	startCmd := exec.Command("docker", "start", s.containerID)
	if out, err := startCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker start: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// Exec 在容器内运行命令。
func (s *DockerSandbox) Exec(ctx context.Context, command string, workdir string) (string, error) {
	s.mu.Lock()
	if s.containerID == "" {
		s.mu.Unlock()
		if err := s.Create(); err != nil {
			return "", err
		}
		s.mu.Lock()
	}
	id := s.containerID
	s.mu.Unlock()

	args := []string{"exec"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args, id, "sh", "-c", command)

	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.CombinedOutput()
	result := string(output)
	if err != nil {
		return fmt.Sprintf("%s\nError: %s", result, err.Error()), err
	}
	return result, nil
}

// ExecWithStdin 在容器内运行命令，并将给定的字节通过管道传入 stdin。
// 用于写入二进制文件（PNG、音频等）——通过 argv 传递原始字节
// （就像我们基于 heredoc 的 WriteFile 所做的那样）会在内容包含 NULL 字节时
// 立即报错 "fork/exec: invalid argument"，因为 execve 拒绝 argv 元素中的 NUL。
func (s *DockerSandbox) ExecWithStdin(ctx context.Context, command string, workdir string, stdin io.Reader) (string, error) {
	s.mu.Lock()
	if s.containerID == "" {
		s.mu.Unlock()
		if err := s.Create(); err != nil {
			return "", err
		}
		s.mu.Lock()
	}
	id := s.containerID
	s.mu.Unlock()

	args := []string{"exec", "-i"}
	if workdir != "" {
		args = append(args, "-w", workdir)
	}
	args = append(args, id, "sh", "-c", command)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	output, err := cmd.CombinedOutput()
	result := string(output)
	if err != nil {
		return fmt.Sprintf("%s\nError: %s", result, err.Error()), err
	}
	return result, nil
}

// Close 停止并移除容器。
func (s *DockerSandbox) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.containerID == "" {
		return nil
	}

	cmd := exec.Command("docker", "rm", "-f", s.containerID)
	cmd.CombinedOutput() // best effort
	s.containerID = ""
	return nil
}

// ContainerID 返回当前容器 ID，如果尚未创建则返回空。
func (s *DockerSandbox) ContainerID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.containerID
}
