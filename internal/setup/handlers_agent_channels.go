package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/channels"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/scope"
	"github.com/qs3c/bkclaw/internal/store"
)

// 按 agent 的 IM 频道 CRUD。包装现有的 scope.SaveChannel + 绑定设置，
// 以便仪表板可以将"连接 Telegram"呈现为一次点击，而不是要求用户手动连接两个单独的配置行。

// channelOut 是 GET /api/agents/<id>/channels 返回的线格式。
// 每 (channelType, accountID) 一行；botToken 被屏蔽。
//
// Source 区分此绑定的位置：
//   - "agent" — agent 的"官方"频道（对具有读取权限的每个用户可见；仅 agent 拥有者/管理员可以变更）
//   - "user"  — 调用者在此 agent 上的每个用户叠加层（仅调用者可见并可变更）
type channelOut struct {
	Type        string `json:"type"`
	AccountID   string `json:"accountId"`
	BotUsername string `json:"botUsername,omitempty"`
	BotToken    string `json:"botToken"` // masked
	Enabled     bool   `json:"enabled"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	Source      string `json:"source,omitempty"`
}

// resolveChannelBindingScope 授权连接/断开调用并返回频道行应写入的 (userID, agentID) 元组。
// 每个频道行现在直接携带绑定者的 user_id — 不再有"agent 官方"的重载，
// 因为两个共享公共 agent 的用户无论如何不能共享同一个 bot（入站消息会路由到错误的空间）。
//
//   - Agent 的拥有者（或平台管理员）：(callerUID, agentID)。
//     拥有者的绑定只是他们在自己 agent 上的个人绑定。
//   - 具有读取权限的非拥有者（公共 agent 或 apikey ACL 授权）：
//     (callerUID, agentID)。每个用户可以将自己的 bot 绑定到同一个公共 agent 而不会与他人冲突，
//     入站消息路由到绑定者的 UserSpace。
//
// 在权限/查找失败时写入 4xx 并返回 ok=false。
//
// 旧的返回形状是 (scope, scopeID, ok)。新形状是 (userID, agentID, ok)。
// 旧调用者将 (sc, scopeID) 传入 invalidateScope；此文件底部的迁移适配器保持其正常工作。
func (s *Server) resolveChannelBindingScope(w http.ResponseWriter, r *http.Request, agentID string) (userID, aid string, ok bool) {
	if agentID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agent id required"})
		return "", "", false
	}
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return "", "", false
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return "", "", false
	}
	ident, _ := auth.FromContext(r.Context())
	if rec.UserID == uid || (ident.AuthMethod != "" && ident.CanAdminPlatform()) {
		return uid, agentID, true
	}
	// 非拥有者：必须至少能够读取 agent。
	if (ident.AuthMethod == "apikey" && ident.CanAccessAgent(agentID)) || rec.IsPublic {
		return uid, agentID, true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "not your agent"})
	return "", "", false
}

// ownsAgent 门控频道管理调用。当调用者是 agent 拥有者或平台管理员（super_admin 会话、type=admin apikey）
// 时返回 (callerUID, true)。绑定/频道行以 agent 为键，因此返回的 uid 是调用者的，而不是拥有者的 —
// 这对每个调用者的流程（如 WeChat QR 会话，其轮询端的相等性检查需要与存储它的起始端匹配）很重要。
func (s *Server) ownsAgent(r *http.Request, agentID string) (string, bool) {
	if agentID == "" {
		return "", false
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		return "", false
	}
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		return "", false
	}
	if rec.UserID == uid {
		return uid, true
	}
	if ident, ok := auth.FromContext(r.Context()); ok && ident.CanAdminPlatform() {
		return uid, true
	}
	return "", false
}

func (s *Server) handleListAgentChannels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	rec, err := s.dataStore.GetAgent(r.Context(), id)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	caller := s.effectiveUserID(r)
	_ = rec // kept around in case future logic gates on agent ownership again

	// 频道行始终携带绑定者的 user_id + 目标 agent_id，因此调用者的视角是"我绑定到此 agent 的内容"—
	// (user_id=caller, agent_id=id)。第一个 ListConfigs 覆盖拥有者在其 agent 上的绑定以及非拥有者的每个 (user, agent) 叠加层。
	//
	// 我们额外拉取 (user_id='', agent_id=id) 以兼容那些重构前的"scope=agent"行逃脱了迁移回填的旧安装
	//（例如当时没有 user_id 的 agent）。新行永远不会写入那里。
	out := make([]channelOut, 0)
	if caller != "" {
		if rows, err := s.dataStore.ListConfigs(r.Context(), store.KindChannel, caller, id); err == nil {
			out = append(out, flattenChannelRows(rows, "agent", "", "")...)
		}
	}
	if rows, err := s.dataStore.ListConfigs(r.Context(), store.KindChannel, "", id); err == nil {
		out = append(out, flattenChannelRows(rows, "agent", "", "")...)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"channels": out})
}

// flattenChannelRows 将每个配置行展开为每个 (channelType, accountID) 一个 channelOut。
// source 标记行的来源，供 UI 渲染徽章。(botToken, _, _) 三元组目前未使用 —
// 保留为可变参数样式，以便将来的调用者可以在不增加额外参数的情况下预屏蔽。
type accountFilter func(channelType, accountID string) bool

func filterAccounts(allow map[[2]string]bool) accountFilter {
	return func(channelType, accountID string) bool {
		return allow[[2]string{channelType, accountID}]
	}
}

func flattenChannelRows(rows []store.ConfigRecord, source string, _, _ string, filters ...accountFilter) []channelOut {
	out := make([]channelOut, 0, len(rows))
	for _, rec := range rows {
		cc := decodeChannelConfigFromRecord(&rec)
		if len(cc.Accounts) == 0 {
			if len(filters) > 0 && !filters[0](rec.Name, "") {
				continue
			}
			out = append(out, channelOut{
				Type:      rec.Name,
				AccountID: "",
				BotToken:  maskAPIKey(cc.BotToken),
				Enabled:   rec.Enabled,
				UpdatedAt: rec.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
				Source:    source,
			})
			continue
		}
		for accountID, acct := range cc.Accounts {
			if len(filters) > 0 && !filters[0](rec.Name, accountID) {
				continue
			}
			tok := acct.BotToken
			if tok == "" {
				tok = cc.BotToken
			}
			out = append(out, channelOut{
				Type:        rec.Name,
				AccountID:   accountID,
				BotUsername: accountID,
				BotToken:    maskAPIKey(tok),
				Enabled:     rec.Enabled,
				UpdatedAt:   rec.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
				Source:      source,
			})
		}
	}
	return out
}

type connectTelegramRequest struct {
	BotToken string `json:"botToken"`
}

// handleConnectAgentTelegram 通过调用 Telegram 的 getMe 验证 bot token，
// 然后持久化一个作用域到此 agent 的 kind=channel + binding 对，并热启动适配器使 bot 立即开始轮询。
func (s *Server) handleConnectAgentTelegram(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectTelegramRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	token := strings.TrimSpace(req.BotToken)
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "botToken required"})
		return
	}

	// 通过 Telegram getMe 验证；这也给我们 bot 用户名，用作绑定的 accountID。
	username, err := telegramGetMe(token)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 构建频道配置：一个以 bot 用户名为键的 Account，以便如果用户稍后添加另一个 bot，
	// 同一 agent 上支持多 bot 设置。每个账户有各自的 BotToken。
	cc := config.ChannelConfig{
		Enabled:  true,
		Accounts: map[string]config.AccountConfig{username: {BotToken: token}},
	}
	// credential_key 必须等于 Telegram 适配器作为 InboundMessage.AccountID 提供的值 —
	// 这是 processInbound 用于查找拥有用户的列（LookupChannelByCredential）。适配器使用
	// accountID = Accounts 映射键创建，即 bot 的 @username，因此我们在此镜像它。
	// 使用 token-tail 回退（credentialKeyFor）会静默丢弃所有入站消息，因为没有匹配的行。
	credKey := username
	if err := s.assertChannelCredentialUnique(r, "telegram", credKey, "", uid, aid); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, uid, aid, "telegram", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// 追加一个绑定，以便入站消息路由到此 agent。现有绑定（例如之前的 Discord bot）被保留。
	// 条目内的 AgentID 始终是路径解析的 agent（= sc=Agent 时的 scopeID；sc=User 时调用者要绑定的外部 agent）。
	if err := s.appendBinding(r, "", "", config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "telegram", AccountID: username},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.invalidateOwner(uid, aid)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "telegram", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"botUsername": username,
	})
}

func (s *Server) handleDisconnectAgentChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	channelType := r.PathValue("type")
	accountID := r.PathValue("accountId")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	// 在解析的作用域（拥有者/管理员为 agent，非拥有者叠加层为 user）中找到频道行。
	// 通过行内 Accounts 映射中的 accountID 进行匹配。
	rows, err := s.dataStore.ListConfigs(r.Context(), store.KindChannel, uid, aid)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	for _, rec := range rows {
		if rec.Name != channelType {
			continue
		}
		cc := decodeChannelConfigFromRecord(&rec)
		_, hasAcct := cc.Accounts[accountID]
		// 当行没有 Accounts 映射时，将其视为传统的单 bot 形式；accountID 必须为空才能匹配。
		if !hasAcct && !(len(cc.Accounts) == 0 && accountID == "") {
			continue
		}
		if hasAcct {
			delete(cc.Accounts, accountID)
		}
		// 如果没有剩余内容，删除该行；否则重写它。
		if len(cc.Accounts) == 0 && (cc.BotToken == "" || hasAcct) {
			if err := s.dataStore.DeleteConfig(r.Context(), rec.ID); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		} else {
			if err := scope.SaveChannelByScope(r.Context(), s.dataStore, rec.LegacyScope(), rec.LegacyScopeID(), rec.Name, rec.CredentialKey, rec.Enabled, cc); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		// 也删除匹配的绑定。
		if err := s.removeBinding(r, "", "", id, channelType, accountID); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.invalidateOwner(uid, aid)
		s.hotUnregisterChannel(channelType, accountID)
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	jsonResponse(w, http.StatusNotFound, map[string]any{"error": "binding not found"})
}

// appendBinding / removeBinding 曾经维护一个承载 (channel, accountID → agentID) 映射的
// kind=setting, name=bindings 行。频道行现在直接携带 agent_id，因此间接层已消失 —
// 保留这些存根以便许多连接/断开处理程序不必同时改变形状。
//
// 迁移删除所有现有的 kind=setting/name=bindings 行，
// 运行时端（gateway/userspace.go::bindingsFromChannelRows）通过直接遍历频道行重建路由表。
func (s *Server) appendBinding(r *http.Request, sc, scopeID string, b config.Binding) error {
	_ = r
	_ = sc
	_ = scopeID
	_ = b
	return nil
}

func (s *Server) removeBinding(r *http.Request, sc, scopeID, agentID, channelType, accountID string) error {
	_ = r
	_ = sc
	_ = scopeID
	_ = agentID
	_ = channelType
	_ = accountID
	return nil
}

// telegramGetMe 通过调用 Bot API 验证 bot token。成功时返回 bot 的用户名。
// 我们避免在此引入 tgbotapi，这样此处理程序不会为了一个 HEAD 样式的验证而拖入完整的长期轮询 bot 机制。
func telegramGetMe(token string) (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", token)
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("contact telegram: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Telegram 在 token 无效时返回 {"ok":false,"description":"..."}。
		var apiErr struct {
			Description string `json:"description"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Description != "" {
			return "", fmt.Errorf("telegram rejected token: %s", apiErr.Description)
		}
		return "", fmt.Errorf("telegram getMe: HTTP %d", resp.StatusCode)
	}
	var ok struct {
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &ok); err != nil {
		return "", fmt.Errorf("parse telegram response: %w", err)
	}
	if ok.Result.Username == "" {
		return "", errors.New("telegram getMe returned empty username")
	}
	return ok.Result.Username, nil
}

// --- Discord ---

type connectDiscordRequest struct {
	BotToken string `json:"botToken"`
}

// handleConnectAgentDiscord 通过调用 Discord REST API 的 /users/@me 验证 Discord bot token，
// 然后持久化 kind=channel + binding 行，就像 Telegram 流程一样。accountID = bot 用户 ID
//（Discord 的稳定标识符，与可以更改的用户名不同）。
func (s *Server) handleConnectAgentDiscord(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectDiscordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	token := strings.TrimSpace(req.BotToken)
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "botToken required"})
		return
	}

	userID, username, err := discordGetMe(token)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// accountID = Discord 用户 ID。在用户名更改时保持稳定，
	// 并与 Discord 适配器在 InboundMessage.AccountID 中提供的值匹配（它从相同的值设置）。
	cc := config.ChannelConfig{
		Enabled:  true,
		Accounts: map[string]config.AccountConfig{userID: {BotToken: token}},
	}
	credKey := userID
	if err := s.assertChannelCredentialUnique(r, "discord", credKey, "", uid, aid); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, uid, aid, "discord", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, "", "", config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "discord", AccountID: userID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateOwner(uid, aid)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "discord", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"botUsername": username,
		"botUserId":   userID,
	})
}

// discordGetMe 通过 Discord REST API 验证 bot token。
// 端点文档：GET /users/@me，带有 `Authorization: Bot <token>` 返回 bot 用户对象
//（id, username, discriminator）。我们避免在此引入 discordgo，
// 这样此处理程序不会仅仅为了检查 token 而打开网关连接。
func discordGetMe(token string) (string, string, error) {
	req, err := http.NewRequest("GET", "https://discord.com/api/v10/users/@me", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bot "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("contact discord: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Discord 在认证错误时返回 {"message": "...", "code": ...}。
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
			return "", "", fmt.Errorf("discord rejected token: %s", apiErr.Message)
		}
		return "", "", fmt.Errorf("discord users/@me: HTTP %d", resp.StatusCode)
	}
	var me struct {
		ID            string `json:"id"`
		Username      string `json:"username"`
		Discriminator string `json:"discriminator"`
		Bot           bool   `json:"bot"`
	}
	if err := json.Unmarshal(body, &me); err != nil {
		return "", "", fmt.Errorf("parse discord response: %w", err)
	}
	if me.ID == "" {
		return "", "", errors.New("discord users/@me returned empty id")
	}
	if !me.Bot {
		return "", "", errors.New("token belongs to a user account, not a bot — connect a bot token from the Discord Developer Portal")
	}
	display := me.Username
	if me.Discriminator != "" && me.Discriminator != "0" {
		// 旧的 Discord 用户名格式为 user#1234。现代（2023 年后）账户的鉴别器为 "0" — 仅显示用户名。
		display = me.Username + "#" + me.Discriminator
	}
	return me.ID, display, nil
}

// --- Slack ---

type connectSlackRequest struct {
	BotToken string `json:"botToken"`
	AppToken string `json:"appToken"`
}

// handleConnectAgentSlack 在通过 auth.test 验证后持久化 Slack bot+app token 对。
// Slack 需要两者：bot token (xoxb-...) 用于发布/读取，app token (xapp-...) 用于 Socket Mode WS。
// accountID = team_id，因此一个工作区的事件都路由到同一 agent（按工作区的 Slack 应用是常见形式）。
func (s *Server) handleConnectAgentSlack(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectSlackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	botToken := strings.TrimSpace(req.BotToken)
	appToken := strings.TrimSpace(req.AppToken)
	if botToken == "" || appToken == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "botToken and appToken both required"})
		return
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "botToken should start with xoxb-"})
		return
	}
	if !strings.HasPrefix(appToken, "xapp-") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "appToken should start with xapp- (app-level token from Settings → Basic Information)"})
		return
	}

	teamID, teamName, botUserID, err := slackAuthTest(botToken)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// Slack 频道行将两个 token 放在顶级的 BotToken/AppToken 中（Slack 适配器构造函数将它们作为一对读取）。
	// Accounts 映射以 team_id 为键，以便入站端通过 LookupChannelByCredential(channel="slack", credKey=teamID) 解析拥有者。
	cc := config.ChannelConfig{
		Enabled:  true,
		BotToken: botToken,
		AppToken: appToken,
		Accounts: map[string]config.AccountConfig{teamID: {BotToken: botToken}},
	}
	credKey := teamID
	if err := s.assertChannelCredentialUnique(r, "slack", credKey, "", uid, aid); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, uid, aid, "slack", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, "", "", config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "slack", AccountID: teamID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateOwner(uid, aid)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "slack", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":        true,
		"teamName":  teamName,
		"teamId":    teamID,
		"botUserId": botUserID,
	})
}

// slackAuthTest 使用 bot token 调用 Slack 的 auth.test 端点以验证它并一次性捕获
// team_id/team_name/bot_user_id。文档：https://api.slack.com/methods/auth.test
func slackAuthTest(botToken string) (teamID, teamName, botUserID string, err error) {
	req, rerr := http.NewRequest("POST", "https://slack.com/api/auth.test", nil)
	if rerr != nil {
		return "", "", "", rerr
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		return "", "", "", fmt.Errorf("contact slack: %w", derr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Slack 总是返回 200 + JSON 体；`ok` 字段携带实际结果。
	var ok struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Team   string `json:"team"`
		TeamID string `json:"team_id"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
	}
	if jerr := json.Unmarshal(body, &ok); jerr != nil {
		return "", "", "", fmt.Errorf("parse slack response: %w", jerr)
	}
	if !ok.OK {
		msg := ok.Error
		if msg == "" {
			msg = "unknown error"
		}
		return "", "", "", fmt.Errorf("slack rejected token: %s", msg)
	}
	if ok.TeamID == "" {
		return "", "", "", errors.New("slack auth.test returned empty team_id")
	}
	return ok.TeamID, ok.Team, ok.UserID, nil
}

// --- 微信 （iLink） ---
//
// 与 Telegram/Discord/Slack 不同，微信不接收粘贴的 token。
// 用户使用微信手机应用扫描二维码；确认后 iLink 返回一个 (bot_token, ilink_bot_id,
// ilink_user_id, baseurl) 元组。两步流程：
//
//   POST /api/agents/{id}/channels/wechat/login
//     → 从 iLink 获取 QR token，在客户端渲染为图片。返回 {sessionID, qrCode, qrCodeImg}。
//
//   GET  /api/agents/{id}/channels/wechat/login/status?session=<id>
//     → 轮询 iLink 的 get_qrcode_status 一次往返。
//       返回 {status: wait|scaned|confirmed|expired, connected, accountId?}。
//       在 `confirmed` 时，持久化频道行 + 绑定并热注册适配器，
//       因此客户端下次轮询沙箱/agent 状态时显示 bot 在线。

const (
	wechatILinkBase    = "https://ilinkai.weixin.qq.com"
	wechatQRCodeURL    = wechatILinkBase + "/ilink/bot/get_bot_qrcode?bot_type=3"
	wechatQRStatusURL  = wechatILinkBase + "/ilink/bot/get_qrcode_status?qrcode="
	wechatStatusWait   = "wait"
	wechatStatusScaned = "scaned"
	wechatStatusOK     = "confirmed"
	wechatStatusExpire = "expired"
)

// wechatLoginSession 跟踪进行中的二维码扫描。仅存在于内存中 —
// 废弃的会话通过注册表上的 TTL 扫描进行垃圾回收。保存到存储会让轮询在进程重启后仍然存活，
// 但 QR token 本身在 iLink 服务器端几分钟后就会过期，因此跨重启恢复不值得这么复杂。
type wechatLoginSession struct {
	qrCode    string // iLink token, used both as polling key + as QR payload
	qrCodeImg string // optional pre-rendered QR image (base64 or URL)
	agentID   string // which agent the credentials should bind to
	userID    string // initiating caller — verified on every status poll
	scope     string // resolved storage scope ("agent" for owner/admin,
	// "user" for non-owner overlay) — captured at start so persist on
	// confirm uses the same scope the caller initially saw.
	scopeID   string
	createdAt time.Time
}

type wechatLoginRegistry struct {
	mu       sync.Mutex
	sessions map[string]*wechatLoginSession
}

var wechatLogins = &wechatLoginRegistry{sessions: map[string]*wechatLoginSession{}}

func (r *wechatLoginRegistry) put(id string, s *wechatLoginSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = s
	// 机会性 GC：丢弃超过 5 分钟的会话（QR 码在此服务器端之前早已过期；任何更旧的都已死亡）。
	cutoff := time.Now().Add(-5 * time.Minute)
	for k, v := range r.sessions {
		if v.createdAt.Before(cutoff) {
			delete(r.sessions, k)
		}
	}
}

func (r *wechatLoginRegistry) get(id string) *wechatLoginSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *wechatLoginRegistry) delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

// handleStartAgentWeChatLogin 向 iLink 请求新的 QR 码，注册一个以返回的 qrCode token 为键的服务器端会话，
// 并将客户端渲染 QR 图像所需的内容返回。实际扫描在用户的微信手机应用中带外进行；
// 然后客户端轮询 handleAgentWeChatLoginStatus 来驱动状态机。
func (s *Server) handleStartAgentWeChatLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	qr, err := wechatFetchQRCode(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	sessionID := qr.QRCode // iLink's token is unique enough; reuse it
	wechatLogins.put(sessionID, &wechatLoginSession{
		qrCode:    qr.QRCode,
		qrCodeImg: qr.QRCodeImgContent,
		agentID:   id,
		userID:    uid,
		// `scope`/`scopeID` 保留在结构体上以与轮询处理程序向后兼容 —
		// 重新用作 (userID, agentID)，以便持久化时使用调用者在开始时看到的相同拥有者。
		scope:     uid,
		scopeID:   aid,
		createdAt: time.Now(),
	})
	jsonResponse(w, http.StatusOK, map[string]any{
		"sessionId": sessionID,
		"qrCode":    qr.QRCode,
		"qrCodeImg": qr.QRCodeImgContent,
	})
}

// handleAgentWeChatLoginStatus 轮询 iLink 获取此会话 QR 码的当前扫描状态。
// 在 `confirmed` 时，持久化频道行 + 绑定 + 热注册适配器 — 与 Telegram / Discord / Slack
// 连接处理程序相同的形状，但由 QR 状态机驱动，而不是立即验证 token。
func (s *Server) handleAgentWeChatLoginStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.resolveChannelBindingScope(w, r, id); !ok {
		return
	}
	uid := s.effectiveUserID(r)
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "session required"})
		return
	}
	sess := wechatLogins.get(sessionID)
	if sess == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found or expired"})
		return
	}
	// 跨租户防护：即使猜到 sessionID，也不让一个用户的轮询观察到另一个用户的 QR 会话。
	if sess.userID != uid || sess.agentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}

	status, err := wechatPollQRStatus(r.Context(), sess.qrCode)
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	switch status.Status {
	case wechatStatusOK:
		// 用户在手机上确认。iLink 返回了凭证；持久化 + 绑定 + 热注册，然后丢弃进行中的会话。
		creds := wechatCredentials{
			BotToken:    status.BotToken,
			ILinkBotID:  status.ILinkBotID,
			BaseURL:     status.BaseURL,
			ILinkUserID: status.ILinkUserID,
		}
		if creds.BotToken == "" || creds.ILinkBotID == "" {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"error": "ilink confirmed without credentials"})
			return
		}
		if err := s.persistWeChatAccount(r, sess.scope, sess.scopeID, id, creds); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		wechatLogins.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{
			"status":    "confirmed",
			"connected": true,
			"accountId": creds.ILinkBotID,
		})
		return
	case wechatStatusExpire:
		wechatLogins.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{
			"status":    "expired",
			"connected": false,
		})
		return
	default:
		// wait / scaned / unknown — 继续轮询。我们明确显示 "scaned"，
		// 因为当用户已点击 QR 但尚未在手机上按确认时，UI 会切换到 "扫描完成,请确认"。
		jsonResponse(w, http.StatusOK, map[string]any{
			"status":    status.Status,
			"connected": false,
		})
		return
	}
}

// persistWeChatAccount 为新确认的 iLink 账户写入 kind=channel 行 + 绑定。
// 旧的第三/四个参数 (sc, scopeID) 现在被重用为携带 (userID, agentID) —
// 微信 QR 流程在开始时将它们存储在 wechatLoginSession 上，以便持久化时使用相同的拥有者。
func (s *Server) persistWeChatAccount(r *http.Request, userID, agentIDArg, agentID string, creds wechatCredentials) error {
	cc := config.ChannelConfig{
		Enabled: true,
		Accounts: map[string]config.AccountConfig{
			creds.ILinkBotID: {
				BotToken: creds.BotToken,
				BaseURL:  creds.BaseURL,
				UserID:   creds.ILinkUserID,
			},
		},
	}
	credKey := creds.ILinkBotID
	if err := s.assertChannelCredentialUnique(r, "wechat", credKey, "", userID, agentIDArg); err != nil {
		return err
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, userID, agentIDArg, "wechat", credKey, true, cc); err != nil {
		return err
	}
	if err := s.appendBinding(r, "", "", config.Binding{
		AgentID: agentID,
		Match:   config.Match{Channel: "wechat", AccountID: creds.ILinkBotID},
	}); err != nil {
		return err
	}
	s.invalidateOwner(userID, agentIDArg)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "wechat", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	return nil
}

// --- iLink HTTP 辅助函数（仅验证；运行中的适配器有自己的客户端在 internal/channels/wechat.go） ---

type wechatCredentials struct {
	BotToken    string
	ILinkBotID  string
	BaseURL     string
	ILinkUserID string
}

type wechatQRCodeResp struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

type wechatQRStatusResp struct {
	Status      string `json:"status"`
	BotToken    string `json:"bot_token"`
	ILinkBotID  string `json:"ilink_bot_id"`
	BaseURL     string `json:"baseurl"`
	ILinkUserID string `json:"ilink_user_id"`
}

func wechatFetchQRCode(ctx context.Context) (*wechatQRCodeResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wechatQRCodeURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact ilink: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ilink qrcode HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out wechatQRCodeResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse ilink qrcode: %w", err)
	}
	if out.QRCode == "" {
		return nil, errors.New("ilink returned empty qrcode")
	}
	return &out, nil
}

// wechatPollQRStatus 执行一次往返 — 返回服务器当前的状态。
// 我们不在服务器端长轮询，因为上游端点已经在做了（约 40 秒）；
// 在每个状态请求上都这样做意味着标签页刷新会卡住 40 秒。
// 客户端改为每 3 秒轮询一次，镜像 workany-web 的形状。
func wechatPollQRStatus(ctx context.Context, qrcode string) (*wechatQRStatusResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wechatQRStatusURL+qrcode, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact ilink: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ilink status HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out wechatQRStatusResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse ilink status: %w", err)
	}
	return &out, nil
}

// --- 飞书 ---

type connectFeishuRequest struct {
	AppID             string `json:"appId"`
	AppSecret         string `json:"appSecret"`
	VerificationToken string `json:"verificationToken"`
	EncryptKey        string `json:"encryptKey"`
	UseLongConn       bool   `json:"useLongConn"`
}

// handleConnectAgentFeishu 通过生成 tenant_access_token（证明 app_id+app_secret 有效）
// 并获取 /bot/v3/info（捕获 bot 的显示名称）来验证飞书自定义应用凭证三元组。
// 将三元组存储为 kind=channel + binding 行 + 热注册适配器。
//
// 存储布局镜像 slack/wechat：credKey = app_id（同时也是 accountID），
// AccountConfig.BotToken = app_secret，AccountConfig.UserID = verification_token
//（与字段的"额外账户作用域标识符"注释匹配）。
func (s *Server) handleConnectAgentFeishu(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectFeishuRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	appID := strings.TrimSpace(req.AppID)
	appSecret := strings.TrimSpace(req.AppSecret)
	verificationToken := strings.TrimSpace(req.VerificationToken)
	encryptKey := strings.TrimSpace(req.EncryptKey)
	useLongConn := req.UseLongConn
	if appID == "" || appSecret == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "appId and appSecret required"})
		return
	}
	if !strings.HasPrefix(appID, "cli_") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "appId should start with cli_ (Feishu custom-app App ID)"})
		return
	}

	botName, botOpenID, err := channels.FeishuValidateCredentials(r.Context(), appID, appSecret)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	cc := config.ChannelConfig{
		Enabled: true,
		Accounts: map[string]config.AccountConfig{
			appID: {
				BotToken:    appSecret,
				UserID:      verificationToken, // 参见 channels/feishu.go 字段映射说明
				EncryptKey:  encryptKey,
				UseLongConn: useLongConn,
			},
		},
	}
	credKey := appID
	if err := s.assertChannelCredentialUnique(r, "feishu", credKey, "", uid, aid); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, uid, aid, "feishu", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, "", "", config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "feishu", AccountID: appID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateOwner(uid, aid)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "feishu", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	resp := map[string]any{
		"ok":          true,
		"appId":       appID,
		"botName":     botName,
		"botOpenId":   botOpenID,
		"useLongConn": useLongConn,
	}
	// Webhook URL 仅在用户选择了公网 URL 传输方式时才有意义。长连接账户不需要它
	//（无需公网入口）— 省略以免 UI 显示用户不能/不应该做的步骤。
	if !useLongConn {
		resp["webhookUrl"] = feishuWebhookPathFor(r, appID)
	}
	jsonResponse(w, http.StatusOK, resp)
}

// feishuWebhookPathFor 构建用户应粘贴到飞书开发者控制台"事件订阅 → 请求 URL"字段的 URL。
// 尽力而为 — 使用请求的 Host，以便反向代理部署显示面向用户的主机名而不是绑定地址。
func feishuWebhookPathFor(r *http.Request, appID string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host + "/api/feishu/webhook/" + appID
}

// --- LINE ---

type connectLINERequest struct {
	ChannelToken  string `json:"channelToken"`
	ChannelSecret string `json:"channelSecret"`
}

// handleConnectAgentLINE 通过使用频道访问 token 访问 /v2/bot/info 来验证 LINE Messaging API 频道。
// 捕获 bot 的 userId（用作 accountID）+ 显示名称 + basicId。
// 将 channel_access_token 存储在 AccountConfig.BotToken，channel_secret 存储在 AccountConfig.UserID
//（与微信/飞书适配器使用的字段映射约定匹配）。
//
// channel_secret 在技术上是可选的 — 适配器可以在没有签名验证的情况下运行 —
// 但 webhook 流量经过开放互联网，因此我们强烈建议设置它。
// 连接处理程序接受空字符串，仅在 secret 缺失时在验证时发出警告。
func (s *Server) handleConnectAgentLINE(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	uid, aid, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectLINERequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	channelToken := strings.TrimSpace(req.ChannelToken)
	channelSecret := strings.TrimSpace(req.ChannelSecret)
	if channelToken == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "channelToken required"})
		return
	}

	userID, displayName, basicID, err := channels.LINEValidateCredentials(r.Context(), channelToken)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	cc := config.ChannelConfig{
		Enabled: true,
		Accounts: map[string]config.AccountConfig{
			userID: {
				BotToken: channelToken,
				UserID:   channelSecret,
			},
		},
	}
	credKey := userID
	if err := s.assertChannelCredentialUnique(r, "line", credKey, "", uid, aid); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, uid, aid, "line", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, "", "", config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "line", AccountID: userID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateOwner(uid, aid)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "line", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":         true,
		"botUserId":  userID,
		"botName":    displayName,
		"basicId":    basicID,
		"webhookUrl": lineWebhookPathFor(r, userID),
	})
}

// lineWebhookPathFor 返回用户粘贴到 LINE Developers Console 中"Messaging API → Webhook URL"的 URL。
// 与 feishuWebhookPathFor 形状相同 — 通过通常的反向代理头部显示面向公众的主机。
func lineWebhookPathFor(r *http.Request, userID string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host + "/api/line/webhook/" + userID
}
