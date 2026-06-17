package session

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/store"
)

// Session 保存 (channel, accountID, chatID) 三元组内一个对话线程的消息历史。
// session_key 是每个会话的不透明 ID；三元组标识对话"在哪里"。
type Session struct {
	mu               sync.Mutex
	Messages         []provider.Message
	LastConsolidated int // index of last consolidated message
	filePath         string
	snapshot         []provider.Message // undo snapshot
	store            SessionStore
	userID           string
	agentID          string
	sessionKey       string
	channel          string
	accountID        string
	chatID           string
	// projectID 在非空时，会标记到该会话的每次 SaveSession 写入中。
	// 在带有项目提示（URL `?project=<pid>`）的全新聊天的第一轮对话中设置；
	// 对于现有行，通过 Manager.Get 读回并在此延迟绑定，以便下次保存时保留它。
	projectID string
	// chatterUserID 是每轮对话参与者 —— 当 IM 通道将每个发送者的
	// app_user 路由到通道拥有者的 UserSpace 时，它与 userID（UserSpace 拥有者 = 通道绑定者）
	// 不同。由智能体循环通过 SetChatter 每轮设置，使 ctx() 将其嵌入到
	// DBStore 会话写入中（sessions.chatter_user_id /
	// session_messages.chatter_user_id / session_events.chatter_user_id）。
	// 当调用方未绑定对话参与者时为空 —— 写入将列留为 ''，读取方回退到 user_id。
	chatterUserID string

	// 转向：turnDepth 统计此会话中正在进行的 HandleMessage 轮次数
	//（计数器而非布尔值，因此重入/重叠的轮次不会使活跃标志挂起）。
	// steerBuf 保存在轮次中间到达的用户消息；正在运行的 ReAct 循环
	// 在工具迭代之间从中取出消息。两者都由 mu 保护。getByKey 从不触碰这些，
	// 因此 Manager.Get 重新加载（会覆盖 Messages）不会破坏待处理的转向。
	turnDepth int
	steerBuf  []provider.Message
}

// SessionKey 返回此 Session 绑定的不透明 session_key。
// 公开以便每轮对话的基础设施（例如目标作用域工具的工具注册绑定）
// 可以访问正确的行，而无需每次重新解析 (channel, account, chat) 四元组。
func (s *Session) SessionKey() string { return s.sessionKey }

// ctx 返回一个标记了此 Session 用户的上下文，使存储层可以按 user_id 限定 SQL 作用域。
// 当未设置用户时回退到 context.Background()；存储层随后默认为 config.DefaultUserID。
//
// 同时嵌入每轮对话的 chatter（如果已设置），使 DBStore 会话写入
//（sessions.chatter_user_id / session_messages.chatter_user_id /
// session_events.chatter_user_id）可以记录实际的对话参与者。
// user_id 保持 = UserSpace 拥有者；chatter 是附加维度。
// 两个标签是独立的 —— 空的 chatter 只是将列留为 ""。
func (s *Session) ctx() context.Context {
	ctx := context.Background()
	if s.userID != "" {
		ctx = config.WithUserID(ctx, s.userID)
	}
	if s.chatterUserID != "" {
		ctx = store.WithChatterUserID(ctx, s.chatterUserID)
	}
	return ctx
}

// SetChatter 将每轮对话参与者绑定到此 Session，以便下一次
// Append / SaveSession 写入标记 chatter_user_id 列。
// 由智能体循环在每轮开始时从解析的 chatterUID 调用。
// 传入 "" 清除它（下一次写入恢复为 ""，读取方回退到 user_id）。
func (s *Session) SetChatter(uid string) {
	s.mu.Lock()
	s.chatterUserID = uid
	s.mu.Unlock()
}

// Manager 管理单个 (user, agent) 的会话。会话内部通过不透明的 session_key 作为键；
// (channel, accountID, chatID) 三元组是调用方用来定位"用户当前所在的对话线程"的。
// 该三元组的活跃会话是最近更新的行 —— `/new` 创建新会话以重新开始。
//
// SessionStore 是可选的持久化接口（生产环境中基于数据库；单二进制开发安装的
// 仅文件模式下为 nil）。
//
// 两种并行的持久化形态：
//   - GetSession / SaveSession 操作面向 LLM 的工作集（压缩后）。
//     这是智能体循环每轮读写的对象。
//   - AppendMessage / ListMessages 操作仅追加的每轮归档（session_messages 表）。
//     压缩从不触及它，因此 UI 历史/审计读取始终看到原始对话，
//     无论工作集被修剪/汇总了多少次。
type SessionStore interface {
	GetSession(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error)
	SaveSession(ctx context.Context, agentID, sessionKey, channel, accountID, chatID, projectID string, messages []provider.Message) error
	AppendMessage(ctx context.Context, agentID, sessionKey string, msg provider.Message) error
	AppendTurnAnchor(ctx context.Context, agentID, sessionKey string, msg provider.Message) (int64, error)
	ListMessages(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error)
	ListWebSessions(ctx context.Context, agentID string) ([]WebSession, error)
	DeleteSession(ctx context.Context, agentID, sessionKey string) error
	RenameSession(ctx context.Context, agentID, sessionKey, title string) error
	// MoveSession 将会话重新分配到不同的项目（projectID 为 "" 时分离）。
	// 由侧边栏拖放功能使用；工作区文件迁移是调用方的责任。
	MoveSession(ctx context.Context, agentID, sessionKey, projectID string) error
	// ResolveActiveSessionKey 返回 (channel, accountID, chatID) 三元组
	// 最近使用的 session_key，如果没有则返回空字符串。
	ResolveActiveSessionKey(ctx context.Context, agentID, channel, accountID, chatID string) (string, error)
	// LookupSessionTriple 是逆操作 —— 给定 session_key，返回其所属的对话。
	// 当会话不存在时返回 ("","","",nil)（管理器将其视为"尚未存储"）。
	LookupSessionTriple(ctx context.Context, agentID, sessionKey string) (channel, accountID, chatID string, err error)
	// LookupSessionProject 返回会话行上标记的 project_id，松散聊天返回 ""。
	// 由智能体运行时用于将项目上下文传递到入站消息上，使工作区存储和
	// 沙箱都路由到 projects/<pid>/。
	LookupSessionProject(ctx context.Context, agentID, sessionKey string) (string, error)
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	dataDir  string
	store    SessionStore
	userID   string
	agentID  string
}

func NewManager(dataDir string) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		dataDir:  dataDir,
	}
}

// NewManagerWithStoreForUser 是用户作用域的构造函数。调用方必须
// 提供从认证解析的真实 user_id —— 没有回退。
func NewManagerWithStoreForUser(dataDir string, st SessionStore, userID, agentID string) *Manager {
	if userID == "" {
		panic("session.NewManagerWithStoreForUser: userID is required")
	}
	return &Manager{
		sessions: make(map[string]*Session),
		dataDir:  dataDir,
		store:    st,
		userID:   userID,
		agentID:  agentID,
	}
}

// ctx 返回一个标记了此 Manager 用户的上下文，用于存储调用。
func (m *Manager) ctx() context.Context {
	if m.userID == "" {
		return context.Background()
	}
	return config.WithUserID(context.Background(), m.userID)
}

// generateSessionKey 为新的对话线程生成一个不透明的 session_key。
// 无论通道如何，都使用相同的生成器 —— `s-<unix_ms>-<rand>`。
// (channel, accountID, chatID) 三元组存储在专门的列中；
// session_key 字符串本身不再编码通道信息，因此 IM 中的 `/new` 命令
// 可以在同一三元组下生成第二个键而不会冲突。
func generateSessionKey() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	var rand6 [6]byte
	if _, err := cryptorand.Read(rand6[:]); err != nil {
		// 回退到时间派生的字节 —— 一旦时间戳前缀生效，碰撞概率极低
		now := time.Now().UnixNano()
		for i := range rand6 {
			rand6[i] = byte(now >> (i * 8))
		}
	}
	suffix := make([]byte, len(rand6))
	for i, b := range rand6 {
		suffix[i] = alphabet[int(b)%len(alphabet)]
	}
	return fmt.Sprintf("s-%d-%s", time.Now().UnixMilli(), suffix)
}

// resolveOrMintKey 从存储中选择 (channel, accountID, chatID) 的活跃 session_key，
// 或者当尚不存在时生成一个新的（对话的第一条消息）。
// 通道三元组迁移之前的现有行可能带有 `web_<sid>` 或 `wechat_<openid>` 这样的键 ——
// 它们通过回填的三元组匹配，而不是通过解析键，因此旧版格式仍然有效。
//
// 新行生成策略：
//   - web：session_key == chatID。Web 的 chatID *就是*每个对话的标识符
//    （前端每次 "+New chat" 生成一个），因此使其等于 session_key 可保持
//    URL `?session=` 令牌在刷新间稳定 —— 不会出现"第一条消息后 URL 变化"的意外。
//   - 其他所有地方：生成不透明的 `s-<unix_ms>-<rand>`。IM 通道在多个会话中
//     重复使用同一个 chatID（用户的 openid / chat_id），因此 session_key 必须独立，
//     以便 `/new` 可以生成并列行。
func (m *Manager) resolveOrMintKey(channel, accountID, chatID string) string {
	if m.store != nil {
		if k, err := m.store.ResolveActiveSessionKey(m.ctx(), m.agentID, channel, accountID, chatID); err == nil && k != "" {
			return k
		}
	}
	if channel == "web" && chatID != "" {
		return chatID
	}
	return generateSessionKey()
}

// Get 返回或创建 (channel, accountID, chatID) 三元组的活跃会话。
// session_key 在服务端解析而非从输入推导 —— 见 resolveOrMintKey。
//
// projectID 是聊天请求中的"此聊天属于项目 X"提示（URL `?project=<pid>`）。
// 仅在首次保存时有效：如果会话行已存储了 project_id，则以存储为准；
// 如果行是全新的，则此提示会被持久化。
//
// 在多副本部署中（基于存储模式），每次 Get() 都会从存储重新加载 Messages，
// 因此由 pod B 处理的请求可以看到 pod A 的写入。否则每个 pod 的内存缓存
// 会与 Postgres 偏离：跨 pod 写入后的第一次刷新会返回恰好暖缓存的
// pod 本地快照。我们有意覆盖缓存 Session 上的 Messages 而非重新创建结构体，
// 以便瞬态字段（snapshot, LastConsolidated）得以保留。
//
// 基于文件的模式保持缓存优先，因为只有一个进程。
func (m *Manager) Get(channel, accountID, chatID, projectID string) *Session {
	key := m.resolveOrMintKey(channel, accountID, chatID)
	return m.getByKey(key, channel, accountID, chatID, projectID)
}

// GetByKey 通过 session_key 加载特定会话。当调用方已经持有键时使用
//（例如从 URL `?session=…` 获取 Web 历史记录），希望绕过活跃会话查找。
func (m *Manager) GetByKey(sessionKey string) *Session {
	return m.getByKey(sessionKey, "", "", "", "")
}

// LookupSessionProject 返回会话行的 project_id（松散/尚未存储时返回 ""）。
// 由智能体运行时用于填充 InboundMessage.ProjectID，使工作区 IO 路由到 projects/<pid>/。
func (m *Manager) LookupSessionProject(sessionKey string) string {
	if m.store == nil || sessionKey == "" {
		return ""
	}
	pid, err := m.store.LookupSessionProject(m.ctx(), m.agentID, sessionKey)
	if err != nil {
		return ""
	}
	return pid
}

// LookupSessionTriple 转发到存储层的 session_key → 三元组查找。
// 当行不存在时返回 ("","","",nil)，与 SessionStore 实现一致。
// 如果调用方需要区分"没有行"和"三元组为空的行"（例如存储为 nil 的
// 基于文件的开发模式），应先使用 SessionExists。
func (m *Manager) LookupSessionTriple(sessionKey string) (channel, accountID, chatID string, err error) {
	if m.store == nil {
		return "", "", "", nil
	}
	return m.store.LookupSessionTriple(m.ctx(), m.agentID, sessionKey)
}

// SessionExists 报告给定 session_key 下是否已存在会话行。
// 由智能体侧 URL 解析器使用：`?session=…` 令牌可以是规范的 session_key
// 或旧版 web chat_id，查找需要一种轻量方式区分两者。
func (m *Manager) SessionExists(sessionKey string) bool {
	if m.store == nil {
		// 基于文件的模式没有反向查找原语 —— 假设存在，以免旧版 chat_id
		// 回退优先于调用方的意图。后续的 GetByKey 将加载磁盘上的任何内容
		//（空文件 → 空 Session，无害）。
		return true
	}
	msgs, err := m.store.GetSession(m.ctx(), m.agentID, sessionKey)
	return err == nil && msgs != nil
}

// ResolveSessionKey 将 URL 令牌（`?session=…`）转换为规范的 session_key。
// 接受以下任一形式：
//   - 直接是 session_key（ListWebSessions 返回的 ID）
//   - 旧版 web chat_id（旧 URL 和前端在"+New chat"的*第一轮*生成的 ID）
//
// 当无匹配时返回输入不变 —— 调用方的下游加载/保存将创建该行，
// 这对于全新的 Web 聊天是正确的，其中 URL 令牌就是即将存在的 session_key。
func (m *Manager) ResolveSessionKey(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if m.SessionExists(sessionID) {
		return sessionID
	}
	if m.store != nil {
		if k, err := m.store.ResolveActiveSessionKey(m.ctx(), m.agentID, "web", "", sessionID); err == nil && k != "" {
			return k
		}
	}
	return sessionID
}

// OpenNewSession 在同一 (channel, accountID, chatID) 三元组下生成一个全新会话
// 并返回其 session_key。该三元组的下一次 Get 将获取到它（它具有最新的 updated_at）。
// 由 IM 的 `/new` / `/reset` 命令和未来的"开始新对话"UI 功能使用。
func (m *Manager) OpenNewSession(channel, accountID, chatID string) string {
	key := generateSessionKey()
	if m.store != nil {
		// 立即持久化一个空行，使下一条入站消息的活跃会话查找解析到此键，
		// 而非前一个（仍然比不存在更新）行。IM 的 `/new` 始终是松散聊天
		//（project_id=""）；项目聊天由聊天处理程序在第一条消息时延迟创建。
		_ = m.store.SaveSession(m.ctx(), m.agentID, key, channel, accountID, chatID, "", nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Session{
		filePath:   filepath.Join(m.dataDir, key+".jsonl"),
		store:      m.store,
		userID:     m.userID,
		agentID:    m.agentID,
		sessionKey: key,
		channel:    channel,
		accountID:  accountID,
		chatID:     chatID,
	}
	m.sessions[key] = s
	return key
}

func (m *Manager) getByKey(key, channel, accountID, chatID, projectID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[key]; ok {
		if m.store != nil {
			if msgs, err := m.store.GetSession(m.ctx(), m.agentID, key); err == nil {
				s.mu.Lock()
				s.Messages = msgs
				s.mu.Unlock()
			}
		}
		// 在通过 GetByKey 或早期无提示路径创建的缓存条目上延迟绑定
		// 三元组 + 项目。一旦标记，持久化行上的 project_id 即被视为权威 ——
		// 我们只填充空的情况，因此提示不匹配不会覆盖真相。
		if channel != "" || projectID != "" {
			s.mu.Lock()
			if s.channel == "" && channel != "" {
				s.channel, s.accountID, s.chatID = channel, accountID, chatID
			}
			if s.projectID == "" && projectID != "" {
				s.projectID = projectID
			}
			s.mu.Unlock()
		}
		return s
	}

	filePath := filepath.Join(m.dataDir, key+".jsonl")

	s := &Session{
		filePath:   filePath,
		store:      m.store,
		userID:     m.userID,
		agentID:    m.agentID,
		sessionKey: key,
		channel:    channel,
		accountID:  accountID,
		chatID:     chatID,
		projectID:  projectID,
	}

	// 如果可用，从存储（数据库）加载，否则从文件加载
	if m.store != nil {
		msgs, err := m.store.GetSession(m.ctx(), m.agentID, key)
		if err == nil && len(msgs) > 0 {
			s.Messages = msgs
		}
	} else {
		s.load()
	}

	m.sessions[key] = s
	return s
}

// Append 向会话添加消息并持久化。
//
// 基于存储的模式写入两个位置：
//   - SaveSession 覆盖 sessions 表中面向 LLM 的工作集（智能体循环下一轮读取的数组）；
//   - AppendMessage 将新轮次插入 session_messages，即存活于压缩的仅追加归档。
//
// 归档写入尽力而为（失败时记录日志但不暴露）—— 丢失一条归档行可从工作集恢复，
// 我们不希望审计表打嗝时历史记录静默丢弃聊天回复。
// Key 返回此 Session 绑定的不透明 session_key。
// 公开以便需要按会话标记外部记录的调用方（例如使用计费的每会话令牌汇总）
// 不必深入结构体内部。
func (s *Session) Key() string { return s.sessionKey }

func (s *Session) Append(msg provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 如果未提供时间戳，自动设置
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}

	s.Messages = append(s.Messages, msg)

	if s.store != nil {
		s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages)
		if err := s.store.AppendMessage(s.ctx(), s.agentID, s.sessionKey, msg); err != nil {
			fmt.Fprintf(os.Stderr, "session archive append error: %v\n", err)
		}
	} else {
		s.appendToFile(msg)
	}
}

// AppendTurnAnchor 与 Append 等价地把消息加入内存工作集并 SaveSession,但归档行
// 带 turn_status='running' 并返回分配的 seq——供 turn 起点登记锚点、turn 结束时
// 按 (sessionKey, seq) 翻 done。仅用于真正开启一个 turn 的用户消息。
// 无持久化 store 时退化为 Append 语义并返回 (-1, nil)。
func (s *Session) AppendTurnAnchor(msg provider.Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	s.Messages = append(s.Messages, msg)
	if s.store == nil {
		s.appendToFile(msg)
		return -1, nil
	}
	s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages)
	return s.store.AppendTurnAnchor(s.ctx(), s.agentID, s.sessionKey, msg)
}

// ArchivedMessages 返回此会话的完整仅追加历史。
// 当未配置存储或归档为空时回退到内存中的工作集
//（例如基于文件的模式，或归档表存在之前创建的会话）。
func (s *Session) ArchivedMessages() []provider.Message {
	s.mu.Lock()
	store := s.store
	agentID := s.agentID
	sessionKey := s.sessionKey
	s.mu.Unlock()
	if store == nil {
		return s.GetMessages()
	}
	msgs, err := store.ListMessages(s.ctx(), agentID, sessionKey)
	if err != nil || len(msgs) == 0 {
		return s.GetMessages()
	}
	return msgs
}

// GetMessages 返回所有消息的副本。
func (s *Session) GetMessages() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	msgs := make([]provider.Message, len(s.Messages))
	copy(msgs, s.Messages)
	return msgs
}

// BeginTurn 将会话的 HandleMessage 轮次标记为正在进行中。
// 与 EndTurn 配对使用。只有当至少一个轮次活跃时才接受转向消息。
func (s *Session) BeginTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnDepth++
}

// EndTurn 将一轮标记为完成。当最后一个进行中的轮次结束时，
// 它返回仍缓冲的任何转向消息（轮次结束竞争：在循环最终取出后推入的消息）。
// 调用方将剩余消息重新分派为新的一轮。
func (s *Session) EndTurn() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnDepth > 0 {
		s.turnDepth--
	}
	if s.turnDepth > 0 || len(s.steerBuf) == 0 {
		return nil
	}
	leftover := s.steerBuf
	s.steerBuf = nil
	return leftover
}

// PushSteerIfActive 仅当轮次当前正在进行中时缓冲转向消息。
// 当没有活跃轮次时返回 false，因此调用方可以回退到将消息作为
// 正常的新轮次分派。返回值是唯一的真相来源 —— 故意不设单独的
// "正在运行"探测以避免竞争。
func (s *Session) PushSteerIfActive(msg provider.Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnDepth == 0 {
		return false
	}
	s.steerBuf = append(s.steerBuf, msg)
	return true
}

// DrainSteer 原子地返回并清除缓冲的转向消息。
// 正在运行的循环在工具迭代之间调用此方法。
func (s *Session) DrainSteer() []provider.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steerBuf) == 0 {
		return nil
	}
	drained := s.steerBuf
	s.steerBuf = nil
	return drained
}

// UnconsolidatedCount 返回自上次合并以来的消息数量。
func (s *Session) UnconsolidatedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Messages) - s.LastConsolidated
}

// MarkConsolidated 更新合并指针。
func (s *Session) MarkConsolidated(index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastConsolidated = index
}

// ReplaceMessages 用给定的列表替换所有会话消息。
// 在上下文压缩后用于修剪会话。
func (s *Session) ReplaceMessages(msgs []provider.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Messages = make([]provider.Message, len(msgs))
	copy(s.Messages, msgs)
	s.LastConsolidated = 0

	if s.store != nil {
		s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages)
	} else {
		s.rewriteFile()
	}
}

// Clear 重置会话消息。
func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = nil
	s.LastConsolidated = 0
	if s.store != nil {
		s.store.DeleteSession(s.ctx(), s.agentID, s.sessionKey)
	} else {
		os.Remove(s.filePath)
	}
}

func (s *Session) load() {
	f, err := os.Open(s.filePath)
	if err != nil {
		return // 文件尚不存在
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var msg provider.Message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		s.Messages = append(s.Messages, msg)
	}
}

func (s *Session) rewriteFile() {
	dir := filepath.Dir(s.filePath)
	os.MkdirAll(dir, 0o755)

	f, err := os.Create(s.filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session rewrite error: %v\n", err)
		return
	}
	defer f.Close()

	for _, msg := range s.Messages {
		data, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		f.Write(data)
		f.Write([]byte("\n"))
	}
}

func (s *Session) appendToFile(msg provider.Message) {
	dir := filepath.Dir(s.filePath)
	os.MkdirAll(dir, 0o755)

	f, err := os.OpenFile(s.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session persist error: %v\n", err)
		return
	}
	defer f.Close()

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	f.Write(data)
	f.Write([]byte("\n"))
}

// WebSession 保存展示给仪表盘的单个聊天会话的元数据。
// 尽管名称有历史原因，它现在涵盖所有通道 —— Channel 字段
// 告诉调用方应该渲染哪个通道的图标。
//
// ID 是 session_key（行的主键），而非 chat_id。
// 指向 chat_id 的旧 URL 仍然通过智能体侧回退（ResolveSessionKey）解析，
// 因此现有书签不会失效。
type WebSession struct {
	ID        string `json:"id"`
	Channel   string `json:"channel,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	ChatID    string `json:"chatId,omitempty"`
	// ProjectID 将此聊天分组到 per-(user, agent) 项目文件夹下。
	// 空 = 松散聊天。公开以便侧边栏可以按项目对聊天进行分区。
	ProjectID string `json:"projectId,omitempty"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	CreatedAt int64  `json:"createdAt"` // 毫秒级 Unix 时间戳
	UpdatedAt int64  `json:"updatedAt"` // 毫秒级 Unix 时间戳
	// ThumbnailURL 是会话第一轮用户消息中附带的第一个 image_url，
	// 公开以便侧边栏可以显示"图片 + 文本"而不仅仅是多模态聊天的文本标签。
	// 对于开场消息没有图片的会话为空。
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
}

// ListWebSessions 扫描会话文件以查找 Web 聊天会话，并返回包含
// id、标题、预览和时间戳的列表。
func (m *Manager) ListWebSessions() []WebSession {
	if m.store != nil {
		sessions, err := m.store.ListWebSessions(m.ctx(), m.agentID)
		if err == nil {
			return sessions
		}
	}
	pattern := filepath.Join(m.dataDir, "web_*.jsonl")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}

	var sessions []WebSession
	for _, f := range files {
		base := filepath.Base(f)
		// "web_<sessionId>.jsonl" -> "<sessionId>"  // 文件名到会话 ID
		sessionId := strings.TrimPrefix(base, "web_")
		sessionId = strings.TrimSuffix(sessionId, ".jsonl")

		info, err := os.Stat(f)
		if err != nil {
			continue
		}

		// 读取第一条用户消息作为预览
		preview := ""
		thumb := ""
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(fh)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			// 多模态用户轮次将文本存储在 content_parts 中，content 为空 ——
			// 读取两种格式以使预览不会附着到后面的纯文本消息并错误标记会话，
			// 并提取第一个 image_url 以便侧边栏可以显示缩略图。
			var msg struct {
				Role         string                 `json:"role"`
				Content      string                 `json:"content"`
				ContentParts []provider.ContentPart `json:"content_parts"`
			}
			if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Role != "user" {
				continue
			}
			text := msg.Content
			img := ""
			if text == "" {
				var parts []string
				for _, p := range msg.ContentParts {
					if p.Type == "text" && p.Text != "" {
						parts = append(parts, p.Text)
					}
				}
				text = strings.Join(parts, "\n")
			}
			text = provider.StripAttachedPrefix(text)
			for _, p := range msg.ContentParts {
				if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
					img = p.ImageURL.URL
					break
				}
			}
			if text == "" && img == "" {
				continue
			}
			preview = text
			if preview == "" {
				preview = "[image]"
			}
			if len(preview) > 100 {
				preview = preview[:100] + "..."
			}
			thumb = img
			break
		}
		fh.Close()

		if preview == "" {
			continue // 跳过空会话
		}

		// 从元数据文件读取标题，回退到预览
		title := m.readSessionTitle(sessionId)
		if title == "" {
			title = preview
			if len(title) > 60 {
				title = title[:60] + "..."
			}
		}

		sessions = append(sessions, WebSession{
			ID:           sessionId,
			Title:        title,
			Preview:      preview,
			ThumbnailURL: thumb,
			CreatedAt:    info.ModTime().UnixMilli(),
			UpdatedAt:    info.ModTime().UnixMilli(),
		})
	}

	// 按 updatedAt 降序排序（最新的在前）
	for i := 0; i < len(sessions); i++ {
		for j := i + 1; j < len(sessions); j++ {
			if sessions[j].UpdatedAt > sessions[i].UpdatedAt {
				sessions[i], sessions[j] = sessions[j], sessions[i]
			}
		}
	}

	return sessions
}

// resolveWebSessionKey 将 web sessionId（URL `?session=` 令牌，即对话的 chat_id）
// 映射到其当前的 session_key。新行具有不透明的 session_key（与 chat_id 不同）；
// 旧版行仍然使用 `web_<sid>` 格式。当行尚不存在时回退到旧版字面形式，
// 以便基于文件的模式和全新会话在重命名/删除时不会出错。
func (m *Manager) resolveWebSessionKey(sessionId string) string {
	if m.store != nil {
		if k, err := m.store.ResolveActiveSessionKey(m.ctx(), m.agentID, "web", "", sessionId); err == nil && k != "" {
			return k
		}
	}
	return "web_" + sessionId
}

// DeleteSessionByID 解析 URL 令牌（session_key 或旧版 web chat_id）
// 并删除匹配的会话。通道无关 —— 由仪表盘用于删除任何通道的聊天。
func (m *Manager) DeleteSessionByID(sessionId string) error {
	key := m.ResolveSessionKey(sessionId)
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()
	if m.store != nil {
		return m.store.DeleteSession(m.ctx(), m.agentID, key)
	}
	// 基于文件的模式只有 "web_<sid>" 文件名约定；非 web 会话在开发模式中
	// 不会到达此路径，因此 DeleteWebSession 中的旧版回退就足够了。
	return m.DeleteWebSession(sessionId)
}

// RenameSessionByID 解析 URL 令牌并重命名匹配的会话。
func (m *Manager) RenameSessionByID(sessionId, title string) error {
	key := m.ResolveSessionKey(sessionId)
	if m.store != nil {
		return m.store.RenameSession(m.ctx(), m.agentID, key, title)
	}
	return m.RenameWebSession(sessionId, title)
}

// MoveSessionByID 将会话重新分配到不同的项目（projectID 为 "" 时分离）。
// 解析 session_key 或旧版 web chat_id。删除内存缓存条目，使下一次
// Get 重新加载带有新标记的 project_id 的行 —— 如果不删除，打开的聊天
// 即使在侧边栏显示其在新项目下后仍会用旧的 project_id 保存。
//
// 基于文件的模式为空操作（无项目概念）—— 仅运行开发模式的调用方不应到达此路径。
func (m *Manager) MoveSessionByID(sessionId, projectID string) error {
	key := m.ResolveSessionKey(sessionId)
	m.mu.Lock()
	if s, ok := m.sessions[key]; ok {
		s.mu.Lock()
		s.projectID = projectID
		s.mu.Unlock()
	}
	m.mu.Unlock()
	if m.store != nil {
		return m.store.MoveSession(m.ctx(), m.agentID, key, projectID)
	}
	return nil
}

// DeleteWebSession 删除 Web 聊天会话文件及其元数据。
func (m *Manager) DeleteWebSession(sessionId string) error {
	key := m.resolveWebSessionKey(sessionId)

	// 从内存缓存中移除
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()

	if m.store != nil {
		return m.store.DeleteSession(m.ctx(), m.agentID, key)
	}

	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")
	sessionFile := filepath.Join(m.dataDir, "web_"+safeId+".jsonl")
	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	os.Remove(metaFile)
	return os.Remove(sessionFile)
}

// RenameWebSession 为 Web 聊天会话设置自定义标题。
func (m *Manager) RenameWebSession(sessionId, title string) error {
	if m.store != nil {
		return m.store.RenameSession(m.ctx(), m.agentID, m.resolveWebSessionKey(sessionId), title)
	}

	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")
	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	data, _ := json.Marshal(map[string]string{"title": title})
	return os.WriteFile(metaFile, data, 0o644)
}

// readSessionTitle 从会话元数据文件读取标题。
func (m *Manager) readSessionTitle(sessionId string) string {
	safeId := strings.ReplaceAll(sessionId, "/", "_")
	safeId = strings.ReplaceAll(safeId, "..", "_")

	metaFile := filepath.Join(m.dataDir, "web_"+safeId+".meta.json")
	data, err := os.ReadFile(metaFile)
	if err != nil {
		return ""
	}
	var meta struct {
		Title string `json:"title"`
	}
	json.Unmarshal(data, &meta)
	return meta.Title
}

// Snapshot 将当前消息列表保存为恢复点（用于撤销）。
func (s *Session) Snapshot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = make([]provider.Message, len(s.Messages))
	copy(s.snapshot, s.Messages)
}

// Undo 恢复上一个快照。如果不存在快照则返回 false。
func (s *Session) Undo() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		return false
	}
	s.Messages = make([]provider.Message, len(s.snapshot))
	copy(s.Messages, s.snapshot)
	s.snapshot = nil
	s.LastConsolidated = 0
	s.rewriteFile()
	return true
}

// HasSnapshot 如果存在撤销快照则返回 true。
func (s *Session) HasSnapshot() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot != nil
}
