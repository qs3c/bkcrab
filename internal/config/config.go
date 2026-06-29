// Package config 包含运行时配置类型和 ctx 的 user-id 管道。
//
// 没有 bkcrab.json。引导设置（端口、绑定、存储 DSN、沙箱后端）
// 来自 BKCRAB_* 环境变量；用户可见的配置（提供者、渠道、agent 等）
// 存储在数据库中。此处的 Config 结构体是网关在启动时从这些来源
// 组装的内存快照；调用方从不从磁盘读取它。
package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type userIDKey struct{}

// WithUserID 将已解析的 user_id 标记到 ctx 上。认证中间件在验证
// 会话 cookie 或 apikey 后执行此操作；其他代码不应调用。
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFromContext 提取已解析的 user_id，如果没有则返回 ""。
//
// 没有 DefaultUserID 回退。到达存储层而没有真实 user_id 的代码路径
// 是 bug——认证中间件应该已经 401 了该请求，cron tick 应该已经从行中
// 读取了作业的所有者，渠道入口应该已经解析了凭据。在开发过程中通过
// 对空 user_id 的存储调用进行 panic 来捕获这些问题。
func UserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(userIDKey{}).(string); ok {
		return v
	}
	return ""
}

// MustUserIDFromContext 返回已解析的 user_id 或错误。在缺少身份
// 是 500 级别 bug 而非正常流程的处理边界使用此方法。
func MustUserIDFromContext(ctx context.Context) (string, error) {
	uid := UserIDFromContext(ctx)
	if uid == "" {
		return "", errors.New("config: request context has no user_id (auth middleware bug)")
	}
	return uid, nil
}

// MCPServerConfig 保存单个 MCP 服务器的配置。
type MCPServerConfig struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// CronJob 定义加载到网关运行时的定时作业。
type CronJob struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Schedule    string `json:"schedule"`
	OwnerUserID string `json:"ownerUserId,omitempty"`
	AgentID     string `json:"agentId"`
	Channel     string `json:"channel"`
	ChatID      string `json:"chatId"`
	Message     string `json:"message"`
}

// HeartbeatCfg 保存心跳配置。
type HeartbeatCfg struct {
	IntervalMinutes int `json:"intervalMinutes,omitempty"`
}

// StorageCfg 镜像引导存储块，以便从 Config 读取的现有代码无需
// 额外传参即可继续工作。
type StorageCfg struct {
	Type        string `json:"type,omitempty"`
	DSN         string `json:"dsn,omitempty"`
	AutoMigrate bool   `json:"autoMigrate,omitempty"`
}

// ObjectStoreCfg 控制对象存储后端。
type ObjectStoreCfg struct {
	Type         string              `json:"type,omitempty"`
	Local        ObjectStoreLocalCfg `json:"local,omitempty"`
	S3           ObjectStoreS3Cfg    `json:"s3,omitempty"`
	AccountID    string              `json:"accountId,omitempty"`
	AliyunIntern bool                `json:"aliyunInternal,omitempty"`
}

type ObjectStoreLocalCfg struct {
	Root string `json:"root,omitempty"`
}

type ObjectStoreS3Cfg struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	UseSSL    bool   `json:"useSSL"`
}

// ToolProviderCfg 保存单个提供者（例如 "exa"）的凭据/端点。
type ToolProviderCfg struct {
	APIKey   string            `json:"apiKey,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
}

// ToolCategoryCfg 选择哪些提供者支持某个工具类别。
type ToolCategoryCfg struct {
	Primary      string   `json:"primary,omitempty"`
	Fallbacks    []string `json:"fallbacks,omitempty"`
	AutoFallback *bool    `json:"autoFallback,omitempty"`
}

func (c ToolCategoryCfg) FallbackEnabled() bool {
	if c.AutoFallback == nil {
		return true
	}
	return *c.AutoFallback
}

func (c ToolCategoryCfg) Chain() []string {
	var out []string
	if c.Primary != "" {
		out = append(out, c.Primary)
	}
	for _, f := range c.Fallbacks {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// HooksCfg 配置 webhook 入口服务器。
type HooksCfg struct {
	Enabled bool   `json:"enabled,omitempty"`
	Token   string `json:"token,omitempty"`
	Path    string `json:"path,omitempty"`
	Port    int    `json:"port,omitempty"`
}

type PluginsCfg struct {
	Enabled bool                      `json:"enabled"`
	Paths   []string                  `json:"paths,omitempty"`
	Entries map[string]PluginEntryCfg `json:"entries,omitempty"`
}

type PluginEntryCfg struct {
	Enabled bool                   `json:"enabled"`
	Config  map[string]interface{} `json:"config,omitempty"`
}

type TaskQueueCfg struct {
	MaxConcurrent  int `json:"maxConcurrent,omitempty"`
	TaskTimeoutSec int `json:"taskTimeoutSec,omitempty"`
}

// SandboxCfg 保存 agent 的沙箱配置。
//
// Image 是旧版单槽 image/template/snapshot——现在是只读回退。当设置时，
// 每后端字段（DockerImage / E2BTemplate / BoxliteSnapshot）具有权威性，
// 因此在仪表板中切换 Backend 会保留每个后端的上次输入值，而不是覆盖
// 共享槽。消费者应优先使用活动 Backend 的每后端字段，仅在每后端字段
// 为空时回退到 Image（为早于拆分的配置提供迁移路径）。
type SandboxCfg struct {
	Enabled         bool   `json:"enabled"`
	Image           string `json:"image,omitempty"`
	DockerImage     string `json:"dockerImage,omitempty"`
	E2BTemplate     string `json:"e2bTemplate,omitempty"`
	BoxliteSnapshot string `json:"boxliteSnapshot,omitempty"`
	Policy          string `json:"policy,omitempty"`
	Backend         string `json:"backend,omitempty"`
	E2BKey          string `json:"e2bKey,omitempty"`
	// Boxlite（https://github.com/boxlite-ai/boxlite）是一个托管沙箱服务，
	// 遵循 openapi/rest-sandbox-open-api.yaml 中的 REST 规范。
	// BoxliteURL 是完整基础 URL（默认 https://api.boxlite.ai/v1）；
	// BoxliteKey 是直接作为 `Authorization: Bearer <key>` 发送的 apikey
	// （无 OAuth 交换——该路径已在上游移除）。
	// ClientID 为兼容旧配置行而保留，但不再连接到任何功能。Prefix 在
	// 为空时默认为 "default"，因此最小配置只需 (URL, Key)。
	BoxliteURL      string `json:"boxliteUrl,omitempty"`
	BoxliteClientID string `json:"boxliteClientId,omitempty"`
	BoxliteKey      string `json:"boxliteKey,omitempty"`
	BoxlitePrefix   string `json:"boxlitePrefix,omitempty"`
	Network         string `json:"network,omitempty"`
	IdleTTLSec      int    `json:"idleTTLSec,omitempty"`
}

// GatewayAuth 现在是一个薄壳——权威认证状态存储在 users 表
// （cookie 会话）和 apikeys 表（bearer）中。此处的 Token 在运行时未使用；
// 保留在结构体上以使现有 JSON 序列化保持兼容，同时该字段正在从
// 调用方迁移出去。
type GatewayAuth struct {
	Mode  string `json:"mode,omitempty"`
	Token string `json:"token,omitempty"`
}

type GatewayHTTPEndpoints struct {
	ChatCompletions GatewayEndpoint `json:"chatCompletions,omitempty"`
	Agents          GatewayEndpoint `json:"agents,omitempty"`
}

type GatewayEndpoint struct {
	Enabled bool `json:"enabled"`
}

type GatewayHTTP struct {
	Endpoints GatewayHTTPEndpoints `json:"endpoints,omitempty"`
}

// GatewayCfg 保存网关服务器配置。旧版 "mode" 字段已移除——
// 多用户是无条件的。
type GatewayCfg struct {
	Port      int          `json:"port,omitempty"`
	Bind      string       `json:"bind,omitempty"`
	Auth      GatewayAuth  `json:"auth,omitempty"`
	HTTP      GatewayHTTP  `json:"http,omitempty"`
	RateLimit RateLimitCfg `json:"rateLimit,omitempty"`
}

type RateLimitCfg struct {
	RPM int `json:"rpm,omitempty"`
}

type MemoryCfg struct {
	AutoPersist AutoPersistCfg `json:"autoPersist,omitempty"`
	FTS         FTSCfg         `json:"fts,omitempty"`
}

type AutoPersistCfg struct {
	Enabled     bool   `json:"enabled"`
	EveryNTurns int    `json:"everyNTurns,omitempty"`
	Model       string `json:"model,omitempty"`
}

type FTSCfg struct {
	Enabled bool   `json:"enabled"`
	DBPath  string `json:"dbPath,omitempty"`
}

type PrivacyCfg struct {
	PIIScrubbing PIIScrubCfg `json:"piiScrubbing,omitempty"`
}

type PIIScrubCfg struct {
	Enabled bool `json:"enabled"`
}

type SkillsLearnerCfg struct {
	Enabled      bool   `json:"enabled"`
	MinToolCalls int    `json:"minToolCalls,omitempty"`
	Model        string `json:"model,omitempty"`
}

// Config 是内存中的运行时快照。网关在启动时从 BKCRAB_* 环境变量 +
// 数据库（system_settings、providers、channels、agents）组装此结构；
// 调用方从不会将其序列化回写——数据库表是持久化的真实来源。
type Config struct {
	Providers     map[string]ProviderConfig  `json:"providers"`
	Agents        AgentsConfig               `json:"agents"`
	Channels      map[string]ChannelConfig   `json:"channels"`
	Bindings      []Binding                  `json:"bindings,omitempty"`
	Teams         map[string]TeamEntry       `json:"teams,omitempty"`
	MCPServers    map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	CronJobs      []CronJob                  `json:"cronJobs,omitempty"`
	Heartbeat     HeartbeatCfg               `json:"heartbeat,omitempty"`
	Storage       StorageCfg                 `json:"storage,omitempty"`
	Sandbox       SandboxCfg                 `json:"sandbox,omitempty"`
	ToolProviders map[string]ToolProviderCfg `json:"toolProviders,omitempty"`
	Tools         map[string]ToolCategoryCfg `json:"tools,omitempty"`
	ObjectStore   ObjectStoreCfg             `json:"objectStore,omitempty"`
	Hooks         HooksCfg                   `json:"hooks,omitempty"`
	Plugins       PluginsCfg                 `json:"plugins,omitempty"`
	Gateway       GatewayCfg                 `json:"gateway,omitempty"`
	TaskQueue     TaskQueueCfg               `json:"taskQueue,omitempty"`
	Skills        SkillsCfg                  `json:"skills,omitempty"`
	Memory        MemoryCfg                  `json:"memory,omitempty"`
	Privacy       PrivacyCfg                 `json:"privacy,omitempty"`
	SkillsLearner SkillsLearnerCfg           `json:"skillsLearner,omitempty"`
}

// ModelCost 保存模型的定价信息。
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

type ModelEntry struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Reasoning     bool      `json:"reasoning"`
	Input         []string  `json:"input"`
	Cost          ModelCost `json:"cost"`
	ContextWindow int       `json:"contextWindow"`
	MaxTokens     int       `json:"maxTokens"`
}

// ProviderConfig 保存 LLM 提供者的 API 凭据——既用作 agents.config 内的
// JSON 结构，也用作由提供者解析器组装的已解析每（范围、名称）视图。
type ProviderConfig struct {
	APIKey   string       `json:"apiKey"`
	APIBase  string       `json:"apiBase"`
	APIType  string       `json:"apiType,omitempty"`
	AuthType string       `json:"authType,omitempty"`
	Models   []ModelEntry `json:"models,omitempty"`
}

const DefaultContextWindow = 128000

// ResolveContextWindow returns the model context window from provider metadata.
func ResolveContextWindow(providers map[string]ProviderConfig, model string, maxTokens int) int {
	model = strings.TrimSpace(model)
	if model != "" && len(providers) > 0 {
		if contextWindow, ok := resolvePrefixedModelContextWindow(providers, model); ok {
			return contextWindow
		}
		if contextWindow, ok := resolveAnyProviderModelContextWindow(providers, model); ok {
			return contextWindow
		}
	}
	return fallbackContextWindow(maxTokens)
}

func fallbackContextWindow(maxTokens int) int {
	if maxTokens > DefaultContextWindow {
		return maxTokens
	}
	return DefaultContextWindow
}

func resolvePrefixedModelContextWindow(providers map[string]ProviderConfig, model string) (int, bool) {
	for _, providerID := range sortedProviderKeysByLength(providers) {
		prefix := providerID + "/"
		if !strings.HasPrefix(model, prefix) {
			continue
		}
		return resolveProviderModelContextWindow(providers[providerID], strings.TrimPrefix(model, prefix))
	}
	return 0, false
}

func resolveAnyProviderModelContextWindow(providers map[string]ProviderConfig, model string) (int, bool) {
	for _, providerID := range sortedProviderKeys(providers) {
		if contextWindow, ok := resolveProviderModelContextWindow(providers[providerID], model); ok {
			return contextWindow, true
		}
	}
	return 0, false
}

func resolveProviderModelContextWindow(provider ProviderConfig, model string) (int, bool) {
	for _, entry := range provider.Models {
		if entry.ContextWindow <= 0 {
			continue
		}
		if entry.ID == model || entry.Name == model {
			return entry.ContextWindow, true
		}
	}
	return 0, false
}

func sortedProviderKeys(providers map[string]ProviderConfig) []string {
	keys := make([]string, 0, len(providers))
	for key := range providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedProviderKeysByLength(providers map[string]ProviderConfig) []string {
	keys := sortedProviderKeys(providers)
	sort.SliceStable(keys, func(i, j int) bool {
		return len(keys[i]) > len(keys[j])
	})
	return keys
}

// UnmarshalJSON 处理已弃用的 `api` 别名到 `apiType` 的转换。
func (pc *ProviderConfig) UnmarshalJSON(data []byte) error {
	type Alias ProviderConfig
	aux := &struct {
		*Alias
		API string `json:"api,omitempty"`
	}{Alias: (*Alias)(pc)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if pc.APIType == "" && aux.API != "" {
		pc.APIType = aux.API
	}
	return nil
}

type AgentsConfig struct {
	Defaults AgentDefaults `json:"defaults"`
}

type AgentDefaults struct {
	Model             string  `json:"model,omitempty"`
	MaxTokens         int     `json:"maxTokens,omitempty"`
	Temperature       float64 `json:"temperature,omitempty"`
	MaxToolIterations int     `json:"maxToolIterations,omitempty"`
	// MaxParallelToolCalls 限制单次 LLM 响应允许并发执行的工具调用数量。
	// LLM 仍然决定 emit 多少工具；我们只是拒绝同时运行超过此数量的工具。
	// 溢出部分会收到合成的 "deferred — re-issue next round" tool_result，
	// 使模型自然串行化。0 = 无限制（无上限，当前行为）。当下游 API
	// （Brave 免费层 1RPS 等）无法承受并行突发时很有用。
	MaxParallelToolCalls int    `json:"maxParallelToolCalls,omitempty"`
	Thinking             string `json:"thinking,omitempty"`
	PolicyPreset         string `json:"policy,omitempty"`
	// PromptMode 位于此处，以便 agent 范围的 `agents.defaults` 配置行
	// （由 CLI 和仪表板写入）在 userspace 组装时回写到 ResolvedAgent——
	// 参见 gateway/userspace.go 中 agentOverride 应用的位置。
	PromptMode string `json:"promptMode,omitempty"`
	// SplitReplies — 每 agent 覆盖 WeChatCfg.SplitReplies。
	// 在此层为 nil 表示 agent 范围行无意见；有效值回退到系统级
	// WeChatCfg.SplitReplies。
	SplitReplies *bool `json:"splitReplies,omitempty"`
	// AutoPersist — 每 agent 覆盖 MemoryCfg.AutoPersist.Enabled。
	// 使用指针类型的原因与 SplitReplies 相同：区分"操作员未触及它"
	// 和"操作员显式设为 false"。非 nil 时，在 agent 构建时翻转
	// ag.memoryCfg.AutoPersist.Enabled，使 loop.go:2286 处的
	// runPostTurn 检查要么触发后台的 distill-into-USER.md/MEMORY.md
	// 流程，要么跳过它。主要用于聊天机器人模式——该模式策划的工具
	// 白名单中没有 write_file，所以这是 agent 跨会话记住聊天者的
	// 唯一方式。
	AutoPersist *bool `json:"autoPersist,omitempty"`
}

// AgentEntry 是单个 agent 行的内存结构，在解析期间使用。
// UserID 是拥有账户（镜像 agents.user_id）。每 agent 模型覆盖不在此处
// 保存——它们存在于 configs 表的 scope=agent 中，在 userspace 加载期间
// 通过 scope.SettingInto 合并。
type AgentEntry struct {
	ID     string `json:"id"`
	UserID string `json:"userId,omitempty"`
	// Name 镜像 agents.name（操作员给的显示名称），并传递到
	// ResolvedAgent.DisplayName，以便在 IDENTITY.md 为空时系统提示
	// 可以标注回退身份行。
	Name                 string                     `json:"name,omitempty"`
	Workspace            string                     `json:"workspace,omitempty"`
	MaxTokens            int                        `json:"maxTokens,omitempty"`
	Temperature          float64                    `json:"temperature,omitempty"`
	MaxToolIterations    int                        `json:"maxToolIterations,omitempty"`
	MaxParallelToolCalls int                        `json:"maxParallelToolCalls,omitempty"`
	Skills               []string                   `json:"skills,omitempty"`
	MCPServers           map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	AlwaysLoadSkills     []string                   `json:"alwaysLoadSkills,omitempty"`
	Thinking             string                     `json:"thinking,omitempty"`
	Sandbox              SandboxCfg                 `json:"sandbox,omitempty"`
	PolicyPreset         string                     `json:"policy,omitempty"`
	// PromptMode 选择框架系统提示参与的程度以及 LLM 看到的内置工具集。
	// 空 = "agent"（当前默认值）以保持向后兼容。参见 PromptMode* 常量。
	// 每模式的内置工具集硬编码在 builtinAllowForMode（internal/agent/loop.go）
	// 中——按设计通过 Plugin / MCP 扩展，而非每 agent 白名单。
	PromptMode string `json:"promptMode,omitempty"`
	// SplitReplies 覆盖此 agent 的系统级 WeChatCfg.SplitReplies 设置。
	// nil = 继承系统默认值；非 nil = 对此 agent 具有权威性。使用指针（而非
	// bool），因为我们需要区分"操作员未触及它"和"操作员显式关闭它"。
	// agent 使用有效值（覆盖或系统默认值）来决定（1）是否在系统提示中
	// 广告 SplitMessageMarker，以及（2）在 OutboundMessage.AllowSplit 上
	// 标记，以便微信适配器知道是否遵循该标记。
	SplitReplies *bool `json:"splitReplies,omitempty"`
	// AutoPersist 覆盖此 agent 的 MemoryCfg.AutoPersist.Enabled。
	// 与 SplitReplies 相同的指针语义。为 true 时，agent 的 runPostTurn
	// 每隔 N 轮触发一次后台 LLM 调用，将近期消息蒸馏到 USER.md（聊天者
	// 档案）和 MEMORY.md（长期事实）中——这是聊天机器人模式的持久化路径，
	// 因为该模式策划的工具白名单不包括 write_file。
	AutoPersist *bool `json:"autoPersist,omitempty"`
}

// PromptMode 控制 BuildSystemPromptAs 发出哪些框架部分。
// 聊天机器人风格的产品（伴侣、客服、角色扮演）无法继承 agent 形状的指令
// （任务委托、todo 跟踪、工具使用纪律、沙箱规则）而不让其角色特征
// 泛化为通用 AI 助手语调。PromptMode 允许部署在每个 agent 中退出这些部分。
const (
	// PromptModeAgent 发出完整框架提示（任务委托、todo.md、
	// 工具使用纪律、沙箱规则、工作区自更新、调度）。当 PromptMode 为空时的默认值。
	PromptModeAgent = "agent"
	// PromptModeChatbot 保留最小身份脚手架
	// （文件用途 schema、保密性、日期），并丢弃所有 agent 循环指令，
	// 使聊天机器人角色文件（SOUL.md / IDENTITY.md / USER.md / MEMORY.md）
	// 直接塑造行为。
	PromptModeChatbot = "chatbot"
	// PromptModeCustomize 仅发出引导文件（加上日期锚点）。作者负责
	// 在 SOUL.md / IDENTITY.md 中放入所需的任何框架指导——此模式将
	// 舞台完全交给角色文件。
	// （从 PromptModeMinimal 重命名以使意图更明显：您是在自定义系统
	// 提示，而不是要求 bkcrab 提供其内置提示的最小版本。）
	PromptModeCustomize = "customize"
)

// ChannelConfig 保存每渠道的运行时配置。由渠道范围解析器从
// 系统/用户/agent 行构建。
type ChannelConfig struct {
	Enabled  bool                     `json:"enabled"`
	BotToken string                   `json:"botToken,omitempty"`
	AppToken string                   `json:"appToken,omitempty"`
	Accounts map[string]AccountConfig `json:"accounts,omitempty"`
}

type AccountConfig struct {
	BotToken string `json:"botToken,omitempty"`
	// BaseURL 是某些适配器使用的每账号 API 基地址，其上游不是固定主机名
	// （例如微信 iLink 在 QR 确认时发放区域特定 baseurl）。对于
	// Telegram/Discord/Slack 为空——它们都访问固定端点。
	BaseURL string `json:"baseUrl,omitempty"`
	// UserID 是某些适配器在 BotToken 之外需要的额外账号范围标识符
	// （微信 iLink 的 `ilink_user_id`，用作 X-WECHAT-UIN 种子以及
	// typing/getconfig 调用）。不适用时为空。
	UserID string `json:"userId,omitempty"`
	// EncryptKey 是上游可选加密 webhook 载荷的适配器使用的对称密钥
	//（飞书的"加密策略 → Encrypt Key"）。当用户未在上游控制台配置加密
	// 时为空——此时适配器期望明文请求体。
	EncryptKey string `json:"encryptKey,omitempty"`
	// UseLongConn 将入站传输切换为从 bkcrab 向外发起的长连接（WebSocket），
	// 而非平台向公共 webhook POST。目前仅飞书适配器遵守；不提供此模式的
	// 适配器会忽略它。为 true 时，验证/加密密钥未使用（WS 连接通过
	// appID/appSecret 认证），且不需要公共可达 URL。
	UseLongConn bool `json:"useLongConn,omitempty"`
}

type Binding struct {
	AgentID string `json:"agentId"`
	Match   Match  `json:"match"`
}

type Match struct {
	Channel   string `json:"channel"`
	AccountID string `json:"accountId,omitempty"`
	Peer      *Peer  `json:"peer,omitempty"`
}

type Peer struct {
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
}

// AgentFileConfigLoader 是第 3 层 agent 配置的间接点。
// 网关引导将其连接为从数据库的 agents.config 行读取。
var AgentFileConfigLoader func(agentID, home string) (AgentFileConfig, bool) = defaultAgentFileConfigLoader

func defaultAgentFileConfigLoader(_, home string) (AgentFileConfig, bool) {
	if home == "" {
		return AgentFileConfig{}, false
	}
	data, err := os.ReadFile(filepath.Join(home, "agent.json"))
	if err != nil {
		return AgentFileConfig{}, false
	}
	var cfg AgentFileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return AgentFileConfig{}, false
	}
	return cfg, true
}

// AgentFileConfig 是每 agent 行覆盖 JSON（agents.config 列）的 schema。
// 每 agent 的 providers/channels 存储在各自的范围化 DB 表中，不在此持久化。
type AgentFileConfig struct {
	Model                string                     `json:"model,omitempty"`
	MaxTokens            int                        `json:"maxTokens,omitempty"`
	Temperature          float64                    `json:"temperature,omitempty"`
	MaxToolIterations    int                        `json:"maxToolIterations,omitempty"`
	MaxParallelToolCalls int                        `json:"maxParallelToolCalls,omitempty"`
	Workspace            string                     `json:"workspace,omitempty"`
	Skills               SkillsConfig               `json:"skills,omitempty"`
	MCPServers           map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	ToolProviders        map[string]ToolProviderCfg `json:"toolProviders,omitempty"`
	Tools                map[string]ToolCategoryCfg `json:"tools,omitempty"`
	Providers            map[string]ProviderConfig  `json:"providers,omitempty"`
	// PromptMode 在文件配置层镜像 AgentEntry.PromptMode。
	// 非空值覆盖条目级设置。
	PromptMode string `json:"promptMode,omitempty"`
	// SplitReplies 镜像 AgentEntry.SplitReplies。nil = 继承；
	// 非 nil = 对此 agent 具有权威性。
	SplitReplies *bool `json:"splitReplies,omitempty"`
	// AutoPersist 镜像 AgentEntry.AutoPersist。nil = 继承；
	// 非 nil = 对此 agent 具有权威性。
	AutoPersist *bool `json:"autoPersist,omitempty"`
	// Admins 在 IM 渠道中管控写入模式斜杠命令（/new /reset /undo /retry
	// /compact /model /personality）。以渠道名称为键（"discord"、"telegram"、
	// "slack" 等），每个值是允许在该渠道运行这些命令的平台侧用户 ID 列表。
	// 空/缺失列表 = 无门控（任何人都可以运行命令——向后兼容的默认值）。
	//
	// 在 web/api 上，门控会穿透到 msg.UserID == agent owner UUID，
	// 无论此字段如何，因为那些渠道直接携带 BkCrab 身份，不需要每平台白名单。
	Admins map[string][]string `json:"admins,omitempty"`
}

type SkillsConfig struct {
	Disabled   []string `json:"disabled,omitempty"`
	AlwaysLoad []string `json:"alwaysLoad,omitempty"`
}

type SkillsCfg struct {
	Install      SkillsInstallCfg                    `json:"install,omitempty"`
	Entries      map[string]SkillEntryCfg            `json:"entries,omitempty"`
	AgentEntries map[string]map[string]SkillEntryCfg `json:"agentEntries,omitempty"`
	Load         SkillsLoadCfg                       `json:"load,omitempty"`
	AlwaysLoad   []string                            `json:"alwaysLoad,omitempty"`
}

type SkillsInstallCfg struct {
	NodeManager string `json:"nodeManager,omitempty"`
}

type SkillEntryCfg struct {
	Enabled bool              `json:"enabled"`
	APIKey  string            `json:"apiKey,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type SkillsLoadCfg struct {
	ExtraDirs []string `json:"extraDirs,omitempty"`
}

// ResolvedAgent 是单个 agent 的完全合并配置。
type ResolvedAgent struct {
	ID     string
	UserID string
	// DisplayName 镜像 agents.name——操作员给 agent 的人类可读名称
	// （"Bob"、"tdj"、"Sonny"）。在 IDENTITY.md 为空时用作系统提示中
	// 的回退身份行，以避免模型以 "Claude" 自我介绍。
	DisplayName          string
	Home                 string
	Workspace            string
	Model                string
	MaxTokens            int
	ContextWindow        int
	Temperature          float64
	MaxToolIterations    int
	MaxParallelToolCalls int
	Thinking             string
	Skills               SkillsConfig
	MCPServers           map[string]MCPServerConfig
	Sandbox              SandboxCfg
	PolicyPreset         string
	ToolProviders        map[string]ToolProviderCfg
	Tools                map[string]ToolCategoryCfg
	Providers            map[string]ProviderConfig
	// Admins 是每渠道的写入模式斜杠命令管理员白名单。
	// 参见 AgentFileConfig.Admins 了解语义和默认值。
	Admins map[string][]string
	// PromptMode 选择系统提示组装配置文件和 LLM 看到的内置工具集。
	// 参见 AgentEntry.PromptMode 了解语义。空值 = PromptModeAgent。
	PromptMode string
	// SplitReplies — nil = 继承系统 WeChatCfg.SplitReplies，
	// 非 nil = 对此 agent 具有权威性。agent 在发送时将有效值
	// （覆盖或系统默认）标记到每个 OutboundMessage.AllowSplit。
	SplitReplies *bool
	// AutoPersist — nil = 继承系统 MemoryCfg.AutoPersist.Enabled，
	// 非 nil = 对此 agent 具有权威性。驱动 runPostTurn 钩子是否
	// 每 N 轮触发 AutoPersistMemory（LLM 驱动的蒸馏到
	// USER.md/MEMORY.md 流程）。
	AutoPersist *bool
}

func (rc *ResolvedAgent) RefreshModelContextWindow() {
	if rc == nil {
		return
	}
	rc.ContextWindow = ResolveContextWindow(rc.Providers, rc.Model, rc.MaxTokens)
}

type TeamEntry struct {
	Agents        []string `json:"agents"`
	DefaultAgent  string   `json:"defaultAgent,omitempty"`
	GroupBehavior string   `json:"groupBehavior,omitempty"`
}

type TeamConfig struct {
	Name    string            `json:"name"`
	Agents  []string          `json:"agents"`
	Routing map[string]string `json:"routing"`
}

// HomeDir 返回 BkCrab 根目录（默认 ~/.bkcrab）。
// 保存沙箱根目录、本地索引和 FS 物化的 agent 缓存。
func HomeDir() (string, error) {
	if h := os.Getenv("BKCRAB_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bkcrab"), nil
}

// AgentHomeDir 返回 ~/.bkcrab/agents/{agentID}/agent——运行时物化
// agent 身份文件的 FS 缓存目录。agents.id 全局唯一，因此不需要用户命名空间。
func AgentHomeDir(agentID string) (string, error) {
	if agentID == "" {
		return "", errors.New("config.AgentHomeDir: agentID is required")
	}
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "agents", agentID, "agent"), nil
}

// AgentWorkspaceDir 返回 agent 的用户可见产物工作目录：
// ~/.bkcrab/workspaces/<agent_id>/。agents.id 全局唯一，因此不需要
// 用户命名空间；每会话子目录在写入时由工作区存储添加
// （参见 workspace.LocalFS）。
func AgentWorkspaceDir(agentID string) (string, error) {
	if agentID == "" {
		return "", errors.New("config.AgentWorkspaceDir: agentID is required")
	}
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "workspaces", agentID), nil
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// ApplyDefaults 填充 Agents.Defaults 上为零值的旋钮。
func ApplyDefaults(cfg *Config) {
	if cfg.Agents.Defaults.MaxTokens == 0 {
		cfg.Agents.Defaults.MaxTokens = 8192
	}
	if cfg.Agents.Defaults.Temperature == 0 {
		cfg.Agents.Defaults.Temperature = 0.7
	}
	if cfg.Agents.Defaults.MaxToolIterations == 0 {
		cfg.Agents.Defaults.MaxToolIterations = 200
	}
}

// MergedAgentConfig 合并默认值与 agent 条目以生成完全解析的 agent 配置。
func (cfg *Config) MergedAgentConfig(entry AgentEntry) ResolvedAgent {
	home, _ := AgentHomeDir(entry.ID)
	workspace := expandPath(entry.Workspace)
	if workspace == "" {
		workspace, _ = AgentWorkspaceDir(entry.ID)
	}

	resolved := ResolvedAgent{
		ID:                   entry.ID,
		UserID:               entry.UserID,
		DisplayName:          entry.Name,
		Home:                 home,
		Workspace:            workspace,
		Model:                cfg.Agents.Defaults.Model,
		MaxTokens:            cfg.Agents.Defaults.MaxTokens,
		Temperature:          cfg.Agents.Defaults.Temperature,
		MaxToolIterations:    cfg.Agents.Defaults.MaxToolIterations,
		MaxParallelToolCalls: cfg.Agents.Defaults.MaxParallelToolCalls,
		Thinking:             cfg.Agents.Defaults.Thinking,
		Sandbox:              cfg.Sandbox,
		PolicyPreset:         cfg.Agents.Defaults.PolicyPreset,
	}

	if entry.MaxTokens > 0 {
		resolved.MaxTokens = entry.MaxTokens
	}
	if entry.Temperature > 0 {
		resolved.Temperature = entry.Temperature
	}
	if entry.MaxToolIterations > 0 {
		resolved.MaxToolIterations = entry.MaxToolIterations
	}
	if entry.MaxParallelToolCalls > 0 {
		resolved.MaxParallelToolCalls = entry.MaxParallelToolCalls
	}
	if entry.Thinking != "" {
		resolved.Thinking = entry.Thinking
	}
	if entry.Sandbox.Enabled {
		resolved.Sandbox = entry.Sandbox
	}
	if entry.PolicyPreset != "" {
		resolved.PolicyPreset = entry.PolicyPreset
	}
	if entry.PromptMode != "" {
		resolved.PromptMode = entry.PromptMode
	}
	if entry.SplitReplies != nil {
		v := *entry.SplitReplies
		resolved.SplitReplies = &v
	}
	if entry.AutoPersist != nil {
		v := *entry.AutoPersist
		resolved.AutoPersist = &v
	}

	if len(cfg.MCPServers) > 0 {
		resolved.MCPServers = make(map[string]MCPServerConfig, len(cfg.MCPServers))
		for k, v := range cfg.MCPServers {
			resolved.MCPServers[k] = v
		}
	}
	if len(cfg.Providers) > 0 {
		resolved.Providers = make(map[string]ProviderConfig, len(cfg.Providers))
		for k, v := range cfg.Providers {
			resolved.Providers[k] = v
		}
	}
	if len(cfg.ToolProviders) > 0 {
		resolved.ToolProviders = make(map[string]ToolProviderCfg, len(cfg.ToolProviders))
		for k, v := range cfg.ToolProviders {
			resolved.ToolProviders[k] = v
		}
	}
	if len(cfg.Tools) > 0 {
		resolved.Tools = make(map[string]ToolCategoryCfg, len(cfg.Tools))
		for k, v := range cfg.Tools {
			resolved.Tools[k] = v
		}
	}

	if fileCfg, ok := AgentFileConfigLoader(entry.ID, home); ok {
		if fileCfg.Model != "" {
			resolved.Model = fileCfg.Model
		}
		if fileCfg.MaxTokens > 0 {
			resolved.MaxTokens = fileCfg.MaxTokens
		}
		if fileCfg.Temperature > 0 {
			resolved.Temperature = fileCfg.Temperature
		}
		if fileCfg.MaxToolIterations > 0 {
			resolved.MaxToolIterations = fileCfg.MaxToolIterations
		}
		if fileCfg.MaxParallelToolCalls > 0 {
			resolved.MaxParallelToolCalls = fileCfg.MaxParallelToolCalls
		}
		resolved.Skills = fileCfg.Skills
		if len(fileCfg.Admins) > 0 {
			resolved.Admins = make(map[string][]string, len(fileCfg.Admins))
			for ch, ids := range fileCfg.Admins {
				cp := make([]string, len(ids))
				copy(cp, ids)
				resolved.Admins[ch] = cp
			}
		}
		for k, v := range fileCfg.MCPServers {
			if resolved.MCPServers == nil {
				resolved.MCPServers = make(map[string]MCPServerConfig)
			}
			resolved.MCPServers[k] = v
		}
		for k, v := range fileCfg.Providers {
			if resolved.Providers == nil {
				resolved.Providers = make(map[string]ProviderConfig)
			}
			resolved.Providers[k] = v
		}
		for k, v := range fileCfg.ToolProviders {
			if resolved.ToolProviders == nil {
				resolved.ToolProviders = make(map[string]ToolProviderCfg)
			}
			resolved.ToolProviders[k] = v
		}
		for k, v := range fileCfg.Tools {
			if resolved.Tools == nil {
				resolved.Tools = make(map[string]ToolCategoryCfg)
			}
			resolved.Tools[k] = v
		}
		if fileCfg.PromptMode != "" {
			resolved.PromptMode = fileCfg.PromptMode
		}
		if fileCfg.SplitReplies != nil {
			v := *fileCfg.SplitReplies
			resolved.SplitReplies = &v
		}
		if fileCfg.AutoPersist != nil {
			v := *fileCfg.AutoPersist
			resolved.AutoPersist = &v
		}
	}

	resolved.RefreshModelContextWindow()
	return resolved
}

// ResolveAgents 从条目列表构建已解析的 agent 配置。
// 真实来源查找在调用方（DB ListAgents）中进行；此函数只做合并。
func ResolveAgents(cfg *Config, entries []AgentEntry) []ResolvedAgent {
	out := make([]ResolvedAgent, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		out = append(out, cfg.MergedAgentConfig(e))
	}
	return out
}

// LoadTeam 从 FS skills bundle 读取 team.json 文件。
func LoadTeam(path string) (*TeamConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tc TeamConfig
	if err := json.Unmarshal(data, &tc); err != nil {
		return nil, err
	}
	return &tc, nil
}
