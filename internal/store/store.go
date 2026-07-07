// Package store 是 BkCrab 的单一持久化层。数据库
// 是必需的（默认为 MySQL）；没有
// 仅文件的回退方案。每个按用户划分的表都需要一个真实的 users.id 行；
// 尚未解析用户的调用方必须返回 401，而不是发明一个占位符。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound 在行不存在时由 Get* 方法返回。在调用处使用
// errors.Is(err, store.ErrNotFound) 进行检查。
var ErrNotFound = errors.New("store: not found")

// AgentFileMutator atomically transforms one exact agent_files row.
type AgentFileMutator func(current []byte, exists bool) (next []byte, delete bool, err error)

// Store 是所有持久化数据的统一接口。
//
// 表分为三类：
//   - 账户范围（users, web_sessions, apikeys）：以 users.id 为键
//   - agent 范围（agents, agent_files, cron_jobs）：以 agents.id 为键；
//     所有权在 agents.user_id 上
//   - 每个（用户, agent）（sessions）：聊天历史对单个用户是私有的
//   - 范围标记（configs）：行携带（scope, scope_id, kind, name）
type Store interface {
	// --- 用户 ---
	CreateUser(ctx context.Context, u *UserRecord) error
	GetUser(ctx context.Context, id string) (*UserRecord, error)
	GetUserByLogin(ctx context.Context, usernameOrEmail string) (*UserRecord, error)
	GetUserByExternal(ctx context.Context, apikeyID, externalID string) (*UserRecord, error)
	ListUsers(ctx context.Context) ([]UserRecord, error)
	UpdateUser(ctx context.Context, u *UserRecord) error
	DeleteUser(ctx context.Context, id string) error
	CountUsers(ctx context.Context) (int, error)

	// --- Web 会话（登录 cookie）---
	CreateWebSession(ctx context.Context, sess *WebSessionRecord) error
	GetWebSession(ctx context.Context, sid string) (*WebSessionRecord, error)
	DeleteWebSession(ctx context.Context, sid string) error
	DeleteExpiredWebSessions(ctx context.Context, before time.Time) error

	// --- API 密钥（每个用户）---
	ListAPIKeys(ctx context.Context, userID string) ([]APIKeyRecord, error)
	GetAPIKey(ctx context.Context, id string) (*APIKeyRecord, error)
	CreateAPIKey(ctx context.Context, ak *APIKeyRecord) error
	DeleteAPIKey(ctx context.Context, id string) error
	RotateAPIKey(ctx context.Context, id, keyHash, keyPrefix string) error
	LookupAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyRecord, error)

	// --- API 密钥 ↔ agent 权限（多对多）---
	SetAPIKeyAgents(ctx context.Context, apikeyID string, agentIDs []string) error
	ListAPIKeyAgents(ctx context.Context, apikeyID string) ([]string, error)
	APIKeyCanAccessAgent(ctx context.Context, apikeyID, agentID string) (bool, error)

	// --- Agents（原子性；agents.id 全局唯一）---
	ListAgents(ctx context.Context, ownerUserID string) ([]AgentRecord, error)
	GetAgent(ctx context.Context, agentID string) (*AgentRecord, error)
	SaveAgent(ctx context.Context, agent *AgentRecord) error
	DeleteAgent(ctx context.Context, agentID string) error
	ListAllAgents(ctx context.Context) ([]AgentRecord, error)

	// --- 会话（每个用户、每个 agent — 聊天历史是私有的）---
	GetSession(ctx context.Context, userID, agentID, sessionKey string) (*SessionRecord, error)
	SaveSession(ctx context.Context, userID, agentID, sessionKey string, session *SessionRecord) error
	ListSessions(ctx context.Context, userID, agentID string) ([]SessionMeta, error)
	// ListSessionOwnerPairs 返回 sessions 表中每个不同的 (user_id, agent_id)
	// 对。被管理员聊天页面用来发现非拥有者的会话：当聊天者将自己的 bot
	// 绑定到一个公共 agent（或在 web 上给公共 agent 发消息）时，
	// 会话行保存在该聊天者的 user_id 下，而不是 agent
	// 所有者的 user_id 下 —— 因此以拥有者键控的 ListSessions 会遗漏它们。迭代
	// 这些对让管理员视图能够枚举所有拥有聊天历史的 (聊天者, agent) 元组，
	// 无论谁拥有该 agent。
	ListSessionOwnerPairs(ctx context.Context) ([]SessionOwnerPair, error)
	DeleteSession(ctx context.Context, userID, agentID, sessionKey string) error
	RenameSession(ctx context.Context, userID, agentID, sessionKey, title string) error
	// MoveSession 将会话重新分配给不同的项目（当 projectID 为 "" 时
	// 解除关联）。被侧边栏拖放功能使用。工作区文件迁移是
	// 调用方的责任 —— 这只会翻转 sessions.project_id。
	MoveSession(ctx context.Context, userID, agentID, sessionKey, projectID string) error
	// ResolveActiveSessionKey 返回 (channel, accountID, chatID) 三元组
	// 最近更新的 session_key，或 ErrNotFound。被 IM 路由用来选择入站消息
	// 所属的对话线程，而无需强制频道适配器跟踪会话 ID。
	ResolveActiveSessionKey(ctx context.Context, userID, agentID, channel, accountID, chatID string) (string, error)
	// LookupSessionTriple 返回已知 session_key 的 (channel, accountID, chatID)
	// —— 是 ResolveActiveSessionKey 的逆操作。Web 聊天处理器用它来在 URL
	// 只携带 session_key 时恢复 chat_id，以便工作区产物保持在原始对话的
	// 命名空间下，而不是按会话重新键控。
	LookupSessionTriple(ctx context.Context, userID, agentID, sessionKey string) (channel, accountID, chatID string, err error)
	// LookupSessionProject 返回 session_key 的 project_id，如果会话是松散的（无项目）
	// 则返回 ""。被工作区路径解析器用来在挂载沙箱时选择 projects/<id>/
	// 而不是 sessions/<chat>/。
	LookupSessionProject(ctx context.Context, userID, agentID, sessionKey string) (string, error)

	// --- 项目（每个用户、每个 agent — 工作区文件夹分组）---
	//
	// 项目只是 (name, description) 加一个稳定 ID；工作区目录由 ID 派生。
	// 会话通过在创建时设置 project_id 来加入；现有行稍后可以通过更新
	// sessions.project_id 来移动（文件迁移是调用方的问题）。
	// 当任何会话仍然引用该行时，DeleteProject 会阻塞——调用方要么先删除聊天，
	// 要么使用软解除关联（将 project_id 清回 ''）。
	ListProjects(ctx context.Context, userID, agentID string) ([]ProjectRecord, error)
	GetProject(ctx context.Context, userID, agentID, projectID string) (*ProjectRecord, error)
	SaveProject(ctx context.Context, p *ProjectRecord) error
	DeleteProject(ctx context.Context, userID, agentID, projectID string) error
	CountProjectSessions(ctx context.Context, userID, agentID, projectID string) (int, error)

	// --- 会话消息（仅追加的每轮存档）---
	//
	// 将每个 Append 镜像到 session_messages，与 sessions.messages JSONB
	// 工作集分开。AppendSessionMessage 在一个 INSERT 中原子性地分配下一个 seq
	// （COALESCE(MAX(seq),-1)+1），因此调用方不传递 seq。ListSessionMessages
	// 按 seq 升序返回一个会话的所有行——这是完整的历史记录，
	// 不受压缩影响。DeleteSession 级联清理这些记录。
	AppendSessionMessage(ctx context.Context, userID, agentID, sessionKey string, msg SessionMessage) error
	// AppendTurnAnchor 写入一条 turn 起点的用户消息(turn_status='running')
	// 并返回分配的 seq。仅由 turn 起点调用——普通 AppendSessionMessage 的
	// turn_status 默认 ''(非锚点)。seq 在事务内分配,模式同 AppendSessionEvent。
	AppendTurnAnchor(ctx context.Context, userID, agentID, sessionKey string, msg SessionMessage) (int64, error)
	// FinishTurn 把锚点行翻成 turn_status='done'(按主键精确定位,避免认错
	// 上次崩溃残留的僵尸 running 行),并写入本 turn 的工具调用数(供技能提取
	// 按 session 累计判定)。turn 结束时由 runPostTurn 调用。
	FinishTurn(ctx context.Context, userID, agentID, sessionKey string, seq int64, toolCallCount int) error
	// ClaimCadenceBatch 在单个写事务内:统计该 (agent, chatter) 下 turn_status='done'
	// 且 extraction_id IS NULL 的锚点,若 >= n 则生成 uuid、对其中至多 batchCap 条置位
	// extraction_id 并返回 (uuid, 这批 TurnRef)。不足 n 返回 ("", nil, nil)。
	// 事务保证并发 runPostTurn 不会重复认领同一批。
	ClaimCadenceBatch(ctx context.Context, agentID, chatterUserID string, n, batchCap int) (string, []TurnRef, error)
	// ResetExtraction 把某次提取认领的所有行 extraction_id 重置回 NULL,
	// 使它们回到待提取状态(异步提取失败时的补偿回滚)。
	ResetExtraction(ctx context.Context, extractionID string) error
	// ClaimSkillBatch 在单个写事务内:选出该 (agent, session) 下 turn_status='done'
	// 且 skill_extraction_id IS NULL 的锚点(按 created_at,seq 至多 batchCap 条,
	// MySQL/PG 加 FOR UPDATE),若 SUM(tool_call_count) >= minTotal 则生成 uuid、
	// 整批置位 skill_extraction_id 并返回 (uuid, TurnRef 列表);不足返回 ("", nil, nil)。
	// 事务保证并发收尾(同实例异步 post-turn / 直连入口 / 跨实例)不会重复认领。
	ClaimSkillBatch(ctx context.Context, agentID, sessionKey string, minTotal, batchCap int) (string, []TurnRef, error)
	// ResetSkillExtraction 把某次技能认领的所有行 skill_extraction_id 重置回 NULL。
	// 仅基础设施错误(回放失败/LLM 故障)时补偿调用;"判定不提取"视为已消费,不重置。
	ResetSkillExtraction(ctx context.Context, skillExtractionID string) error
	UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error
	RecordSkillLoad(ctx context.Context, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*SkillUsageRow, error)
	ListSkillUsage(ctx context.Context, agentID string) ([]SkillUsageRow, error)
	DeleteSkillUsage(ctx context.Context, agentID, slug string) error
	// LoadTurnMessages 按 TurnRef 列表从归档回放被认领 turn 的消息,按 session 分组返回
	// (每 session 一条查询,锚点边界决定每个 turn 的区间)。供记忆提取按 session 分节拼 prompt。
	LoadTurnMessages(ctx context.Context, userID, agentID string, refs []TurnRef) ([]TurnGroup, error)
	ListSessionMessages(ctx context.Context, userID, agentID, sessionKey string) ([]SessionMessage, error)

	// --- Context archives ---
	//
	// ContextArchiveRecord stores original tool results removed from the LLM
	// working set by compaction. Summaries keep the opaque id; the model can
	// retrieve the exact original by id through the scoped tool.
	SaveContextArchive(ctx context.Context, rec *ContextArchiveRecord) error
	GetContextArchive(ctx context.Context, agentID, sessionKey, id string) (*ContextArchiveRecord, error)

	// --- 聊天事件（进行中的流式增量，持久化用于恢复）---
	//
	// agent 在一轮中发出的每个事件（内容块、tool_call、error、done）
	// 都带有一个按会话自增的 seq 存储在这里。在轮次中断开连接的客户端
	// （刷新、网络闪断、移动应用后台化）使用它们最后看到的 seq 重新连接
	// 并接收错过的增量——没有这个，agent 的回复将不可见，
	// 直到父会话行下次被加载。由 DeleteSession 与 session_messages 一起清除。
	AppendSessionEvent(ctx context.Context, userID, agentID, sessionKey, eventType string, data []byte) (int64, error)
	ListSessionEventsSince(ctx context.Context, userID, agentID, sessionKey string, sinceSeq int64) ([]SessionEventRecord, error)
	LatestSessionEventSeq(ctx context.Context, userID, agentID, sessionKey string) (int64, error)

	// --- Agent 文件 ---
	//
	// SOUL.md、IDENTITY.md、MEMORY.md、AGENTS.md、BOOTSTRAP.md 等。
	// 分层：user_id="" 是共享模板（通过管理员自定义页面编辑），
	// user_id=u_xxx 是该用户的个人覆盖。
	// 读取操作通过回退机制优先选择用户特定的覆盖而非模板；写入操作
	// 精确命中 (agentID, userID, filename) 行。
	// GetAgentFile 优先使用调用方自己的行，回退到 agent
	// 拥有者的行。使用 GetAgentFileExact 进行严格的 (agent, user, filename)
	// 查找，绕过覆盖层。
	GetAgentFile(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	GetAgentFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	SaveAgentFile(ctx context.Context, agentID, userID, filename string, data []byte) error
	MutateAgentFile(ctx context.Context, agentID, userID, filename string, fn AgentFileMutator) ([]byte, error)
	DeleteAgentFile(ctx context.Context, agentID, userID, filename string) error
	ListAgentFiles(ctx context.Context, agentID, userID string) ([]string, error)

	// --- 配置（providers / channels / settings 都在这里）---
	//
	// 一个表支持所有三个概念家族。每行以 (kind, user_id, agent_id, name) 为键，
	// 并携带一个 JSON `data` 负载。
	//
	//   kind="provider"：LLM 提供商（name = 提供商密钥，例如 "openai"）
	//   kind="channel"：频道适配器（name = 频道类型，例如 "telegram"）
	//   kind="setting"：配置命名空间（name = "agents.defaults", "sandbox", …）
	//
	// `credential_key` 仅为 kind="channel" 填充——它是入站调度器在消息到达时
	// 用来查找行的稳定查找键。`enabled` 让一行可以在合并中隐藏外部范围的行
	// （被频道使用：内部范围禁用的行会擦除外部的条目）。
	//
	// ListConfigs(kind, userID, agentID) 返回精确匹配 BOTH ID 的行。
	// 将任一 ID 留空以过滤对应的所有权维度。两者都留空只获取系统/全局行。
	ListConfigs(ctx context.Context, kind, userID, agentID string) ([]ConfigRecord, error)
	// ListConfigsByUser 返回由 userID 拥有的给定 kind 的每一行，
	// 无论 agent_id 如何。UserSpace 组装使用这个来展示调用方是
	// 外部 agent 上的绑定者的频道行——ListConfigs(kind, userID, ownedAgentID)
	// 遗漏的行，因为循环只访问用户拥有的 agent。传递 userID="" 以获取
	// 系统范围的行（等同于 ListConfigs(kind, "", "")）。
	ListConfigsByUser(ctx context.Context, kind, userID string) ([]ConfigRecord, error)
	// QueryAllConfigs 返回给定 kind 的每一行，无论所有权如何。
	// 被网关启动路径用来注册磁盘上的每个频道适配器，也被管理员工具
	// 用来列出跨用户/agent 的某个 kind 的所有行。
	QueryAllConfigs(ctx context.Context, kind string) ([]ConfigRecord, error)
	GetConfig(ctx context.Context, id string) (*ConfigRecord, error)
	GetConfigByName(ctx context.Context, kind, userID, agentID, name string) (*ConfigRecord, error)
	SaveConfig(ctx context.Context, c *ConfigRecord) error
	DeleteConfig(ctx context.Context, id string) error
	LookupChannelByCredential(ctx context.Context, channelType, credKey string) (*ConfigRecord, error)
	GetMCPGatewayRuntime(ctx context.Context, userID string) (*MCPGatewayRuntimeRecord, error)
	SaveMCPGatewayRuntime(ctx context.Context, rec *MCPGatewayRuntimeRecord) error
	ListMCPGatewayRuntimesByStatus(ctx context.Context, statuses ...string) ([]MCPGatewayRuntimeRecord, error)

	// --- Cron 任务（每个 agent）---
	//
	// Cron 行由 agent 拥有；执行身份是 agent 的 user_id。
	// 按 ownerUserID 列出时与 agents 表进行连接。
	ListCronJobsByOwner(ctx context.Context, ownerUserID string) ([]CronJobRecord, error)
	ListCronJobsByAgent(ctx context.Context, agentID string) ([]CronJobRecord, error)
	GetCronJob(ctx context.Context, jobID string) (*CronJobRecord, error)
	SaveCronJob(ctx context.Context, job *CronJobRecord) error
	DeleteCronJob(ctx context.Context, jobID string) error
	GetDueCronJobs(ctx context.Context, now time.Time) ([]CronJobRecord, error)
	LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error)
	UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error
	// IncrementCronJobFailure 原子性地增加 failure_count 并返回新计数。
	// 被调度器在某个 tick 无法投递到配置的频道时使用；调用方决定是否
	// 在达到阈值时删除该行。
	IncrementCronJobFailure(ctx context.Context, jobID string) (int, error)
	// GetNextDueTime 返回所有启用的 cron 任务中最早的 next_run。
	// 被调度器用来精确休眠直到下一个任务到期，而不是轮询。
	GetNextDueTime(ctx context.Context) (time.Time, error)

	// --- 频道租约（轮询频道的单例门控）---
	//
	// 跨进程领导者选举，针对一个 (channel, account_id) 对。
	// AcquireChannelLease 在以下情况返回 true：新获取、通过获取续约、
	// 或在到期后抢占。RenewChannelLease 在租约丢失时返回 false
	// （不是错误）——调用方必须立即停止底层轮询器以避免重复的入站投递。
	// ReleaseChannelLease 删除该行，以便对等方可以在不等待 TTL 的情况下接管。
	AcquireChannelLease(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error)
	RenewChannelLease(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error)
	ReleaseChannelLease(ctx context.Context, channel, accountID, holderID string) error

	// --- 目标（每个 agent × 会话）---
	//
	// 每个 (agent_id, session_key) 最多一行；由 UNIQUE 索引强制执行。
	// 当该对已被占用时，CreateGoal 返回 ErrGoalAlreadyExists；
	// 调用方必须先 DeleteGoal 才能开始新的目标。
	CreateGoal(ctx context.Context, g *GoalRecord) error
	GetGoalBySession(ctx context.Context, agentID, sessionKey string) (*GoalRecord, error)
	// UpdateGoal 写回可变字段。调用方不可变字段
	//（ID、AgentID、SessionKey、OwnerUserID、Objective、CreatedAt）被忽略。
	UpdateGoal(ctx context.Context, g *GoalRecord) error
	DeleteGoal(ctx context.Context, goalID string) error

	Close() error
}

// ErrGoalAlreadyExists 在 (agent_id, session_key) UNIQUE 约束触发时由
// CreateGoal 返回。调用方将其转换为用户可见的"请先清除现有目标"错误。
var ErrGoalAlreadyExists = errors.New("goal already exists for this session")

// UserRecord 是 users 表中的一行。
//
// 角色："super_admin" | "user" 是通过密码/令牌登录的第一方人类。
// "app_user" 由 api_key 代表下游应用进行配置；对于这些行，
// APIKeyID 标识了创建它们的密钥，ExternalID 是调用应用自己的用户
// 标识符（自由格式）。它们共同为每个外部最终用户提供了一个稳定的
// bkcrab user_id，无需任何人登录。
type UserRecord struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email"`
	PasswordHash string `json:"-"`
	DisplayName  string `json:"displayName,omitempty"`
	Role         string `json:"role"`   // "super_admin" | "user" | "app_user"
	Status       string `json:"status"` // "active" | "disabled"
	APIKeyID     string `json:"apikeyId,omitempty"`
	ExternalID   string `json:"externalId,omitempty"`
	// AvatarURL 是一个自包含的 data: URL（"data:image/png;base64,..."）
	// 内联存储以避免单独的 blob 路径。大小限制由处理程序在写入时强制执行
	// （默认为 256KB）。空值表示"无头像"——UI 回退到首字母。
	AvatarURL string `json:"avatarUrl,omitempty"`
	// AgentQuota 限制此用户可以通过 POST /api/agents 自行创建的 agent 数量。
	// 语义：
	//   -1（默认）— 无限制
	//    0        — 禁止自行创建（例如由管理员配置 agent 的单租户客户）
	//    N > 0    — 同时最多拥有 N 个 agent
	// 管理员配置路径（POST /api/admin/users/{id}/agents）绕过此限制——
	// 配额仅控制由调用方发起的创建。
	AgentQuota int64     `json:"agentQuota"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// WebSessionRecord 支持基于 cookie 的登录状态。
type WebSessionRecord struct {
	SID       string    `json:"sid"`
	UserID    string    `json:"userId"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// APIKeyRecord 是 apikeys 表中的一行。KeyHash 是 SHA256(token)；
// 明文在创建/轮换时仅向调用方显示一次。
//
// Type 是密钥的权限层级：
//   - "admin"：完整平台——颁发用户，管理提供商/模型/技能
//   - "user"：apikey 拥有者自己的资源——可以创建 agent，
//     访问 apikey 拥有者拥有的每个 agent（在认证时解析）
//   - "agent"：锁定到 apikey_agents 中的显式列表——不能创建 agent
type APIKeyRecord struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	Name      string    `json:"name,omitempty"`
	KeyHash   string    `json:"-"`
	KeyPrefix string    `json:"keyPrefix,omitempty"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"createdAt"`
}

// AgentRecord 是单个 agent 的持久化状态。agents.id 全局唯一；
// UserID 是拥有该 agent 的用户。agent 本身是原子单元——
// 会话、cron 任务和 apikey ACL 都直接引用 agents.id，
// 从不引用 (user_id, agent_id)。
// 每个 agent 的模型覆盖曾经存在于 agents.model；现在它们存在于 configs 中，
// 作为 kind=setting, scope=agent, scope_id=<aid>, name="agents.defaults"，
// 这与系统 + 用户默认值采用的路径相同。解析在 loadUserSpace 中通过 scope.SettingInto 完成。
// IsPublic 翻转"任何拥有链接的人都可以聊天"的门控。默认 false
// （私有——仅拥有者）。当为 true 时，requireAgentReadable +
// resolveAgent 让任何经过身份验证的会话将 agent 懒加载到它们自己的
// UserSpace 中；会话/记忆/agent 文件仍然按聊天者分区，
// 因此只有 agent 身份是共享的。
type AgentRecord struct {
	ID        string                 `json:"id"`
	UserID    string                 `json:"userId"`
	Name      string                 `json:"name"`
	Config    map[string]interface{} `json:"config,omitempty"`
	IsPublic  bool                   `json:"isPublic"`
	CreatedAt time.Time              `json:"createdAt"`
	UpdatedAt time.Time              `json:"updatedAt"`
}

type MCPGatewayRuntimeRecord struct {
	ID                  string    `json:"id"`
	UserID              string    `json:"userId"`
	Status              string    `json:"status"`
	DockerContainerID   string    `json:"dockerContainerId,omitempty"`
	ContainerName       string    `json:"containerName,omitempty"`
	Image               string    `json:"image"`
	InternalPort        int       `json:"internalPort"`
	ExternalPort        int       `json:"externalPort,omitempty"`
	BaseURL             string    `json:"baseUrl,omitempty"`
	APIKey              string    `json:"-"`
	DeployedServersJSON string    `json:"deployedServersJson,omitempty"`
	LastAccessedAt      time.Time `json:"lastAccessedAt"`
	ErrorMessage        string    `json:"errorMessage,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

// SessionRecord 持有一个对话会话。
//
// Channel / AccountID / ChatID 标识此会话所属的上游对话
//（例如 ("wechat", "<bot account id>", "<openid>") 或
// ("web", "", "<frontend session id>")）。这些在 INSERT 时持久化一次，
// 永远不会被 UPDATE 覆盖——会话的归属地在创建后不会移动。
// 多个会话行可以共享相同的三元组；IM 路由的活动会话通过 max(updated_at) 解析。
type SessionRecord struct {
	Channel   string `json:"channel,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	ChatID    string `json:"chatId,omitempty"`
	// ProjectID 对共享工作区文件夹的会话进行分组；空值 =
	// 松散聊天（每个会话有自己的每个聊天的沙箱目录）。
	// 与频道三元组一样，它仅在 INSERT 时持久化并在 UPDATE 时保留。
	ProjectID string           `json:"projectId,omitempty"`
	Messages  []SessionMessage `json:"messages"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// TurnRef 指向一个已认领 turn 的锚点位置:用于从归档表回放该 turn 的消息区间。
type TurnRef struct {
	SessionKey string
	StartSeq   int64
}

// TurnGroup 是一个 session 下被认领 turn 回放出的消息(按 seq 升序),
// 供记忆提取按 session 分节拼 prompt。
type TurnGroup struct {
	SessionKey string
	Messages   []SessionMessage
}

// SessionMessage 是会话中的单条消息。
type SessionMessage struct {
	Role         string                 `json:"role"`
	Content      string                 `json:"content"`
	ContentParts interface{}            `json:"contentParts,omitempty"`
	ToolCalls    interface{}            `json:"toolCalls,omitempty"`
	ToolCallID   string                 `json:"toolCallId,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
	Timestamp    time.Time              `json:"timestamp"`
	Thinking     string                 `json:"thinking,omitempty"`
	RawAssistant json.RawMessage        `json:"rawAssistant,omitempty"`
	// Origin 镜像 provider.Message.Origin — 对真实的用户/助手消息为空，
	// 对运行时注入的消息非空（目前仅有 "goal_context"）。
	// 作为 session_messages 上的列存储（参见 migrateSessionMessagesAddOrigin）。
	Origin string `json:"origin,omitempty"`
}

// ContextArchiveRecord is the durable body behind a compacted tool-result
// summary. Lookup is scoped by (agent_id, session_key, id); user_id is kept for
// audit and cleanup but is not required for retrieval.
type ContextArchiveRecord struct {
	ID            string    `json:"id"`
	UserID        string    `json:"userId,omitempty"`
	AgentID       string    `json:"agentId,omitempty"`
	SessionKey    string    `json:"sessionKey,omitempty"`
	ToolCallID    string    `json:"toolCallId,omitempty"`
	ToolName      string    `json:"toolName,omitempty"`
	Content       string    `json:"content"`
	ContentBytes  int       `json:"contentBytes"`
	ContentSHA256 string    `json:"contentSha256,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// SessionEventRecord 是 session_events 表中的一行——agent 在一轮中发出的
// 单个增量。Data 是不透明的 JSON，其形状取决于 Type
//（"content", "tool_call", "error", "done", ...）。
type SessionEventRecord struct {
	UserID     string    `json:"userId,omitempty"`
	AgentID    string    `json:"agentId,omitempty"`
	SessionKey string    `json:"sessionKey,omitempty"`
	Seq        int64     `json:"seq"`
	Type       string    `json:"type"`
	Data       []byte    `json:"data,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// SessionOwnerPair 是由 ListSessionOwnerPairs 返回的一个 (user_id, agent_id)
// 元组——表示"该用户与此 agent 至少有一个会话。"管理员聊天视图按对展开，
// 拉取每个聊天者的会话列表，以便公共 agent 上的非拥有者对话
//（其中 session.user_id ≠ agent.user_id）被展示出来。
type SessionOwnerPair struct {
	UserID  string `json:"userId"`
	AgentID string `json:"agentId"`
}

// SessionMeta 是会话的摘要信息（用于列表展示）。
type SessionMeta struct {
	Key            string    `json:"key"`
	Channel        string    `json:"channel,omitempty"`
	AccountID      string    `json:"accountId,omitempty"`
	ChatID         string    `json:"chatId,omitempty"`
	ProjectID      string    `json:"projectId,omitempty"`
	Title          string    `json:"title,omitempty"`
	MessageCount   int       `json:"messageCount"`
	UpdatedAt      time.Time `json:"updatedAt"`
	LastTurnStatus string    `json:"lastTurnStatus,omitempty"`
}

// ProjectRecord 是一个按 (user, agent) 划分的命名工作区文件夹。会话通过
// sessions.project_id 引用项目；同一项目中的每个会话都将
// workspaces/<agent>/projects/<id>/ 挂载为它的沙箱 /workspace，
// 因此文件在项目的聊天之间共享。
//
// 项目对其创建者（user_id, agent_id 对）是私有的，
// 与 SessionRecord 的所有权模型匹配——共享同一 agent 的不同用户
// 各有自己的项目，永远不会看到彼此的项目。
type ProjectRecord struct {
	UserID      string    `json:"-"`
	AgentID     string    `json:"-"`
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// ConfigRecord 的 kind 常量。
const (
	KindProvider = "provider"
	KindChannel  = "channel"
	KindSetting  = "setting"
)

// ConfigRecord 是 configs 表中的一行——providers、channels 和命名空间设置
// 的统一存放处。
//
//   - kind 表示此行属于哪个家族
//   - (user_id, agent_id) 表示谁拥有它；空字符串默认值
//     给我们四个自然的所有权级别：
//     (”, ”)   = 系统/全局
//     (X, ”)    = 用户 X 的私有配置
//     (”, Y)    = agent Y 的"官方"配置（任何使用 Y 的人都继承）
//     (X, Y)     = 用户 X 在 agent Y 上的每个 agent 覆盖（多租户）
//   - name 是该家族内的查找句柄（provider 密钥、频道类型或设置命名空间）
//   - data 是家族特定的 JSON 负载
//
// CredentialKey 仅对 kind="channel" 有意义——参见 LookupChannelByCredential。
type ConfigRecord struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Scope 是从 (UserID, AgentID) 派生的反规范化标签。
	// "system" / "user" / "agent" / "user-agent"。存储层是
	// 唯一的写入者——SaveConfig 总是重新计算并覆盖调用方传递的任何值，
	// 因此该列不会与 (user_id, agent_id) 的真相来源不同步。
	// 保留此列以便数据库转储和临时查询（`WHERE scope='system'`）保持可读，
	// 而无需解析 ID 列的空/非空模式。
	Scope         string                 `json:"scope,omitempty"`
	UserID        string                 `json:"userId,omitempty"`
	AgentID       string                 `json:"agentId,omitempty"`
	Name          string                 `json:"name"`
	Enabled       bool                   `json:"enabled"`
	CredentialKey string                 `json:"credentialKey,omitempty"`
	Data          map[string]interface{} `json:"data,omitempty"`
	CreatedAt     time.Time              `json:"createdAt"`
	UpdatedAt     time.Time              `json:"updatedAt"`
}

// computeConfigScope 从 (userID, agentID) 所有权对派生范围标签。
// Scope 列的单一真相来源——SaveConfig 在每次写入前调用它，
// 因此列与标签之间的任何差异都意味着绕过了 SaveConfig 的代码路径
//（除了测试专用的临时 INSERT 外不应存在）。
func computeConfigScope(userID, agentID string) string {
	switch {
	case userID != "" && agentID != "":
		return "user-agent"
	case userID != "":
		return "user"
	case agentID != "":
		return "agent"
	default:
		return "system"
	}
}

// LegacyScope 返回适用于 HTTP 层 (scope, scopeId) JSON 形状的范围标签。
// 当已设置时读取持久化的列；对于在列添加迁移之前存在或通过原始 INSERT
// 在测试中构造的行，回退到重新计算。
func (r ConfigRecord) LegacyScope() string {
	if r.Scope != "" {
		return r.Scope
	}
	return computeConfigScope(r.UserID, r.AgentID)
}

// LegacyScopeID 是 LegacyScope 的 scopeID 部分。对于每个 (user, agent)
// 的行，它返回 "user_id/agent_id"，以便 JSON 消费者有足够的信息进行往返。
func (r ConfigRecord) LegacyScopeID() string {
	switch {
	case r.UserID != "" && r.AgentID != "":
		return r.UserID + "/" + r.AgentID
	case r.UserID != "":
		return r.UserID
	case r.AgentID != "":
		return r.AgentID
	default:
		return ""
	}
}

// GoalRecord 是 /goal 目标的持久化形状。每个 (agent, session) 一个——
// UNIQUE 索引强制执行。参见 internal/agent/goal 了解领域类型和原理。
type GoalRecord struct {
	ID          string `json:"id"`
	AgentID     string `json:"agentId"`
	SessionKey  string `json:"sessionKey"`
	OwnerUserID string `json:"ownerUserId"`

	// 路由元组——参见 goal.Goal 了解原理。延续发布到此地址，
	// 以便提示落地到正确的聊天中。
	Channel   string `json:"channel,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	ChatID    string `json:"chatId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`

	Objective string `json:"objective"`
	Status    string `json:"status"` // active | paused | budget_limited | complete

	// TokenBudget 对于无限目标是 nil。存储为可空的 BIGINT 列。
	TokenBudget *int64 `json:"tokenBudget,omitempty"`
	TokensUsed  int64  `json:"tokensUsed"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CronJobRecord 持有一个计划任务。agent_id 是必需的；user_id 也被存储，
// 这样"列出用户的 cron 任务"就不需要与 agents 表连接，
// 所有权检查也可以短路。
type CronJobRecord struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId,omitempty"`
	AgentID   string     `json:"agentId"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Schedule  string     `json:"schedule"`
	Message   string     `json:"message"`
	Channel   string     `json:"channel"`
	ChatID    string     `json:"chatId"`
	AccountID string     `json:"accountId"`
	Timezone  string     `json:"timezone"`
	Enabled   bool       `json:"enabled"`
	LastRun   *time.Time `json:"lastRun,omitempty"`
	NextRun   *time.Time `json:"nextRun,omitempty"`
	// FailureCount 是目标频道缺失/不可达的连续触发尝试次数。
	// UpdateCronJobRun 将其重置为 0；IncrementCronJobFailure 增加它。
	// 调度器一旦超过内部阈值就删除该行。
	FailureCount int       `json:"failureCount,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// StorageType 标识存储后端。
type StorageType string

const (
	StorageMySQL    StorageType = "mysql"
	StoragePostgres StorageType = "postgres"
	StorageSQLite   StorageType = "sqlite"
)

// StorageConfig 持有数据库凭据。在启动时从 BKCRAB_STORAGE_* 环境变量填充。
type StorageConfig struct {
	Type        StorageType `json:"type"`
	DSN         string      `json:"dsn,omitempty"`
	AutoMigrate bool        `json:"autoMigrate,omitempty"`
}
