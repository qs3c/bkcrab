package bus

// Source 标识 InboundMessage 的来源。默认值（空字符串）表示最终用户
// 通过聊天界面输入的（IM、Web、OpenAI 兼容 API、webhook、插件）——这是
// 最常见的情况，现有调用点无需修改。
//
// 非用户来源是 agent 运行时需要区分的情况：cron 自触发、心跳 ticks、
// 子 agent 生成以及运行时注入的目标续行。特别是 /goal 功能使用此值
// 来判断一次轮次是否因真实用户消息而结束（应触发续行探测），还是因
// 这些合成来源而结束（不应续行，否则会循环）。
const (
	SourceUser        = "" // 默认值——保持 "" 以使现有生产者无需修改即可正常工作
	SourceCron        = "cron"
	SourceHeartbeat   = "heartbeat"
	SourceSubAgent    = "subagent"
	SourceGoalContext = "goal_context"
)

// InboundMessage 表示从渠道接收到的消息。
type InboundMessage struct {
	Channel   string // 渠道类型，例如 "telegram"
	AccountID string // 渠道内的账号（例如哪个机器人）
	ChatID    string // 渠道内唯一的聊天标识符
	// ProjectID 设置后表示该聊天所属的每（用户、agent）项目。空值 = 松散
	// 聊天（旧行为）。由聊天处理器解析会话行后标记在入站消息上，以便 agent
	// 运行时将工作区 I/O 路由到 projects/<id>/ 而不是 sessions/<chat>/。
	ProjectID   string
	UserID      string // 用户标识符
	OwnerUserID string // 拥有该 agent 的 bkclaw 用户（用于多用户路由）
	// AgentID 是一个*显式*的 agent 目标。当消息来源已知应由哪个 agent 处理时
	// 非空（cron 任务、Web 聊天、子 agent 生成）——绕过 routeDM 中的绑定查找
	// + 默认 agent 回退。对于 IM 渠道消息为空，网关必须从绑定中确定 agent。
	AgentID      string
	MessageID    string   // 聊天内唯一的消息标识符
	Text         string   // 消息文本
	PeerKind     string   // "group" 或 "dm"
	SenderName   string   // 发送者的显示名称
	// SenderAvatarURL 是消息发送者在平台侧的头像 URL，当渠道可以提供时
	// 设值（Discord 提供 cdn.discordapp.com/avatars/<user_id>/<hash>.png；
	// Telegram/Slack 需要单独的 API 调用，因此桥接器暂时留空）。作为仅 UI
	// 元数据存储在 session_message 行上——LLM 永远看不到它——以便 Web 聊天
	// 面板可以在每个 IM 路由的用户气泡上渲染头像和昵称标题。
	SenderAvatarURL string
	Mentions     []string // 消息中 @提及的用户名
	IsBotMessage bool     // 如果消息由机器人发送则为 true
	PhotoURL     string   // 附加照片的 URL（如有）——单图遗留字段
	PhotoURLs    []string // 附加照片的 URL 列表。与 PhotoURL 独立，以使旧的
	// 单图调用方（Telegram 桥接等）无需修改即可继续工作；新的 Web 聊天路径
	// 使用此字段处理多图附件。
	ReplyToMsgID string   // 正在回复的消息 ID
	// Params 是由调用客户端提供的自由格式结构化参数块（通常来自聊天补全
	// API 的 `params` 字段）。agent 循环将其渲染为每轮系统消息，以便 LLM
	// 在调用工具时可以遵循它。作用域为每次请求——不存储在会话历史中，
	// 下一轮携带自己的 params（或不携带）。当入站来源不提供 params
	// 时（IM 渠道、Web 聊天）为 nil / 空。
	Params map[string]any
	// Source 区分用户来源的消息与运行时来源的消息（cron、心跳、子 agent、
	// 目标续行）。空值表示"用户"。参见 Source* 常量。agent 循环端读取此值
	// 以决定该轮次是否应触发仅对真实用户输入有效的下游反应。
	Source string
}

// OutboundButton 表示内联键盘中的按钮。
type OutboundButton struct {
	Text         string
	CallbackData string
	URL          string
}

// MediaItem 是在消息到达总线时字节已解析完成（从 workspace.Store / 沙箱
// 快照 / 其他来源读取）的附件。无法访问主机文件系统的渠道（e2b 路径）
// 仍然需要上传到 Telegram/Discord 等平台，因此我们内联发送字节，而不是
// 要求每个渠道适配器持有 workspace.Store 引用。
type MediaItem struct {
	Filename    string // 用于内容类型嗅探和 IM 中显示
	ContentType string // 可选覆盖；渠道可在为空时自行嗅探
	Bytes       []byte
}

// OutboundMessage 表示要发送到渠道的消息。
type OutboundMessage struct {
	Channel      string             // 目标渠道类型
	AccountID    string             // 渠道内的目标账号
	AgentID      string             // 来源 agent——WebChannel 使用它将 SSE 事件路由到正确的（agent, session）对；对 IM 渠道无影响（它们以 AccountID 为键）。
	ChatID       string             // 目标聊天标识符
	Text         string             // 消息文本
	ReplyToMsgID string             // 回复指定消息
	ParseMode    string             // "MarkdownV2"、"HTML" 或 ""
	Buttons      [][]OutboundButton // 内联键盘行
	EditMsgID    string             // 编辑现有消息而非发送新消息
	MediaPaths   []string           // 要附加的文件路径（来自 MEDIA: 协议；仅限主机挂载的后端）
	MediaItems   []MediaItem        // 预解析附件——渠道直接上传字节
	// AllowSplit 在绑定微信的消息上为 true 时，允许适配器遵循
	// SplitMessageMarker 并发出多个气泡。为 false（默认值）时，将标记
	// 折叠为换行，避免杂散标记作为纯文本泄露。由来源 Agent 根据其
	// 有效的 splitReplies 设置（每 agent 覆盖或系统默认）标记——参见
	// internal/agent/loop.go。对非微信渠道无害：它们忽略此字段。
	AllowSplit bool
}

// MessageBus 是一个由 Go channel 支持的异步消息队列。
type MessageBus struct {
	Inbound  chan InboundMessage
	Outbound chan OutboundMessage
}

// New 创建一个带缓冲 channel 的新 MessageBus。
func New() *MessageBus {
	return &MessageBus{
		Inbound:  make(chan InboundMessage, 100),
		Outbound: make(chan OutboundMessage, 100),
	}
}
