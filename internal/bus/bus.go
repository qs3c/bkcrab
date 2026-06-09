package bus

// Source identifies what produced an InboundMessage. The default
// (empty string) means an end-user typed it through a chat surface
// (IM, web, OpenAI-compat API, webhook, plugin) — that's the most
// common case and the existing call sites don't have to be touched.
//
// Non-user sources are the cases the agent runtime needs to
// distinguish: cron self-fires, heartbeat ticks, sub-agent spawns,
// and runtime-injected goal continuations. The /goal feature in
// particular uses this to decide whether a turn just finished
// because of a real user message (and should trigger a continuation
// probe) or because of one of these synthetic ones (which should not,
// or we'd loop).
const (
	SourceUser        = "" // default — keep "" so existing producers stay correct without edits
	SourceCron        = "cron"
	SourceHeartbeat   = "heartbeat"
	SourceSubAgent    = "subagent"
	SourceGoalContext = "goal_context"
)

// InboundMessage represents a message received from a channel.
type InboundMessage struct {
	Channel   string // channel type, e.g. "telegram"
	AccountID string // account within the channel (e.g. which bot)
	ChatID    string // unique chat identifier within the channel
	// ProjectID, when set, names the per-(user, agent) project the chat
	// belongs to. Empty = loose chat (legacy behavior). Stamped on
	// inbound messages by the chat handlers after they've resolved the
	// session row, so the agent runtime can route workspace IO to
	// projects/<id>/ instead of sessions/<chat>/.
	ProjectID   string
	UserID      string // user identifier
	OwnerUserID string // bkclaw user that owns the agent (for multi-user routing)
	// AgentID is an *explicit* agent target. Non-empty when the source
	// of the message already knows which agent should handle it (cron
	// jobs, web chat, sub-agent spawns) — bypasses binding lookup +
	// default-agent fallback in routeDM. Empty for IM-channel messages
	// where the gateway has to figure out the agent from bindings.
	AgentID      string
	MessageID    string   // unique message identifier within the chat
	Text         string   // message text
	PeerKind     string   // "group" or "dm"
	SenderName   string   // display name of the sender
	// SenderAvatarURL is the platform-side avatar URL for the message
	// sender, when the channel can provide one (Discord serves
	// `cdn.discordapp.com/avatars/<user_id>/<hash>.png`; Telegram/Slack
	// need a separate API hit so the bridges leave this empty for now).
	// Stored on the session_message row as UI-only metadata — the LLM
	// never sees it — so the web chat panel can render an avatar +
	// nickname header on each IM-routed user bubble.
	SenderAvatarURL string
	Mentions     []string // @usernames mentioned in the message
	IsBotMessage bool     // true if the message was sent by a bot
	PhotoURL     string   // URL of attached photo (if any) — single-image legacy field
	PhotoURLs    []string // URLs of attached photos. Independent of PhotoURL so old single-image callers (Telegram bridge etc.) keep working untouched; new web-chat path uses this for multi-image attachments.
	ReplyToMsgID string   // message ID being replied to
	// Params is a freeform structured-parameter blob supplied by the
	// calling client (typically a third-party app via the chat
	// completions API's `params` field). The agent loop renders it as
	// a per-turn system message so the LLM can honor it when calling
	// tools. Scope is per-request — not stored in session history,
	// next turn ships its own params (or none). nil / empty when the
	// inbound source doesn't supply params (IM channels, web chat).
	Params map[string]any
	// Source distinguishes user-originated messages from runtime-
	// originated ones (cron, heartbeat, sub-agent, goal continuations).
	// Empty means "user". See the Source* constants. Read this on the
	// agent-loop side to decide whether the turn should fire downstream
	// reactions that are only valid for genuine user input.
	Source string
}

// OutboundButton represents a button in an inline keyboard.
type OutboundButton struct {
	Text         string
	CallbackData string
	URL          string
}

// MediaItem is an attachment whose bytes are already resolved (read
// from workspace.Store / sandbox snapshot / wherever) by the time the
// message lands on the bus. Channels that can't access the host
// filesystem (e2b path) still need to upload to Telegram/Discord/etc.,
// so we ship the bytes inline rather than asking each channel adapter
// to hold a workspace.Store reference.
type MediaItem struct {
	Filename    string // for content-type sniffing + display in IM
	ContentType string // optional override; channels can sniff if empty
	Bytes       []byte
}

// OutboundMessage represents a message to be sent to a channel.
type OutboundMessage struct {
	Channel      string             // target channel type
	AccountID    string             // target account within the channel
	AgentID      string             // originating agent — used by the WebChannel to route SSE events to the right (agent, session) pair; harmless for IM channels (which key on AccountID).
	ChatID       string             // target chat identifier
	Text         string             // message text
	ReplyToMsgID string             // reply to specific message
	ParseMode    string             // "MarkdownV2", "HTML", ""
	Buttons      [][]OutboundButton // inline keyboard rows
	EditMsgID    string             // edit existing message instead of sending new
	MediaPaths   []string           // file paths to attach (from MEDIA: protocol; host-mounted backends only)
	MediaItems   []MediaItem        // pre-resolved attachments — channel uploads bytes directly
	// AllowSplit, when true on a WeChat-bound message, lets the adapter
	// honor SplitMessageMarker and emit multiple bubbles. False (default)
	// collapses the marker to a newline so a stray marker doesn't leak
	// as literal text. Stamped by the originating Agent based on its
	// effective splitReplies setting (per-agent override OR system
	// default) — see internal/agent/loop.go. Harmless on non-WeChat
	// channels: they ignore the field.
	AllowSplit bool
}

// MessageBus is an async message queue backed by Go channels.
type MessageBus struct {
	Inbound  chan InboundMessage
	Outbound chan OutboundMessage
}

// New creates a new MessageBus with buffered channels.
func New() *MessageBus {
	return &MessageBus{
		Inbound:  make(chan InboundMessage, 100),
		Outbound: make(chan OutboundMessage, 100),
	}
}
