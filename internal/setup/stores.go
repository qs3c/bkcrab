package setup

import "github.com/qs3c/bkclaw/internal/store"

// 本文件集中定义各 handler / 服务实际需要的"窄存储接口"。
// 它们都是 store 子接口的组合 —— 单实现 *DBStore 满足全部，
// 但每个消费者只声明依赖自己真正调用的那几个域，而非整个 store.Store。
// 这是 Go "accept interfaces" 惯例的彻底落地：构造函数签名即精确的存储依赖契约。

// agentConfigStore = agents + configs。被 configRepo / channelRepo /
// SessionHandler / AgentChannelsHandler / ScopedHandler 使用 —— 它们既要读写
// 配置行，又要按 agentID 解析归属。
type agentConfigStore interface {
	store.AgentStore
	store.ConfigStore
}

// chatStore = 会话消息存档 + 项目。ChatHandler 用它回放 session_events
// 并解析聊天所属项目。
type chatStore interface {
	store.SessionMessageStore
	store.ProjectStore
}

// agentsStore = agents + configs + users。AgentsHandler 在 CRUD 之外还要
// 读取 per-agent 配置覆盖与拥有者用户名。
type agentsStore interface {
	store.AgentStore
	store.ConfigStore
	store.UserStore
}

// agentFilesStore = agents + agent_files + sessions。AgentFilesHandler 读写
// 系统文件、按归属门控，并把会话 key 解析为 chat_id。
type agentFilesStore interface {
	store.AgentStore
	store.AgentFileStore
	store.SessionStore
}

// usersStore 是管理员用户管理最宽的窄接口：用户 CRUD、嵌套 agent 配置、
// fork agent 文件、跨用户聊天展开。
type usersStore interface {
	store.AgentStore
	store.UserStore
	store.SessionStore
	store.SessionMessageStore
	store.AgentFileStore
	store.ConfigStore
}

// workspaceRepoStore = sessions + agents。workspaceRepo 解析会话/项目作用域
// 并读写 agent 文件配置行。
type workspaceRepoStore interface {
	store.SessionStore
	store.AgentStore
}
