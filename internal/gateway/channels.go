package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/qs3c/bkclaw/internal/bus"
	"github.com/qs3c/bkclaw/internal/channels"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/store"
)

// storeLeaser 适配 store.Store 到 channels.Leaser。方法名称不同
//（channels 侧是 Acquire/Renew/Release，store 侧是 ...ChannelLease）
// 以便 store 可以扩展其他租约类型而无需重命名。放在这里而不是 store 包中，
// 以保持 store 接口与 IM 无关。
type storeLeaser struct{ st store.Store }

func (s storeLeaser) Acquire(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	return s.st.AcquireChannelLease(ctx, channel, accountID, holderID, ttl)
}
func (s storeLeaser) Renew(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	return s.st.RenewChannelLease(ctx, channel, accountID, holderID, ttl)
}
func (s storeLeaser) Release(ctx context.Context, channel, accountID, holderID string) error {
	return s.st.ReleaseChannelLease(ctx, channel, accountID, holderID)
}

// registerChannelInstance 为 configs 中一个 kind="channel" 的行启动通道适配器。
// 行的 credential_key 是 processInbound 通过 Store.LookupChannelByCredential
// 反向查找以找到所有者的方式 — 保持其稳定（例如 bot token 的尾部、app id）。
//
// `hot` 控制 bot 适配器的轮询 goroutine 是否立即启动。启动时注册使用 Register
//（Manager.Start 一次性扇出所有内容）；仪表盘变更使用 RegisterAndStart，
// 以便新保存的 bot 无需重启进程即可开始接收更新。
func registerChannelInstance(rec store.ConfigRecord, mb *bus.MessageBus, chanMgr *channels.Manager, st store.Store, hot bool) error {
	cc := decodeChannelConfig(rec)
	switch rec.Name {
	case "telegram":
		return registerTelegramChannels(cc, mb, chanMgr, hot)
	case "discord":
		return registerDiscordChannels(cc, mb, chanMgr, hot)
	case "slack":
		return registerSlackChannels(cc, mb, chanMgr, hot)
	case "line":
		return registerLINEChannels(cc, mb, chanMgr, hot)
	case "wechat":
		return registerWeChatChannels(rec, cc, mb, chanMgr, st, hot)
	case "feishu":
		return registerFeishuChannels(cc, mb, chanMgr, hot)
	}
	return nil
}

// register 通过适当的路径（启动时 Register vs 热注册 RegisterAndStart）将适配器添加到管理器。
// 保持每个通道的分支整洁。
func register(chanMgr *channels.Manager, ch channels.Channel, hot bool) {
	if hot {
		chanMgr.RegisterAndStart(ch)
		return
	}
	chanMgr.Register(ch)
}

// registerSingleton 是轮询通道变体：运行此二进制文件的每个副本将共享 (channel, accountID) 租约，
// 只有租约持有者的 Start 运行。用于 Telegram 长轮询、微信 iLink 长轮询、
// Discord/Slack WS 和飞书长连接 — 任何第二个并发客户端会传递重复入站消息的情况。
// 仅 webhook 适配器（LINE、飞书 webhook 模式）和进程内扇出（Web）使用普通的 register。
func registerSingleton(chanMgr *channels.Manager, ch channels.Channel, hot bool) {
	if hot {
		chanMgr.RegisterSingletonAndStart(ch)
		return
	}
	chanMgr.RegisterSingleton(ch)
}

func decodeChannelConfig(rec store.ConfigRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, err := json.Marshal(rec.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = rec.Enabled
	return cc
}

func registerTelegramChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		if !chanMgr.ClaimTelegramToken(chCfg.BotToken) {
			slog.Warn("telegram token already registered in this process, skipping duplicate")
			return nil
		}
		tg, err := channels.NewTelegram(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, tg, hot)
		return nil
	}
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		if !chanMgr.ClaimTelegramToken(token) {
			slog.Warn("telegram token already registered in this process, skipping duplicate", "account", accountID)
			continue
		}
		tg, err := channels.NewTelegram(token, accountID, mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, tg, hot)
	}
	return nil
}

func registerDiscordChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		dc, err := channels.NewDiscord(chCfg.BotToken, "", mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, dc, hot)
		return nil
	}
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		dc, err := channels.NewDiscord(token, accountID, mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, dc, hot)
	}
	return nil
}

func registerSlackChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	if len(chCfg.Accounts) == 0 {
		sl, err := channels.NewSlack(chCfg.BotToken, chCfg.AppToken, "", mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, sl, hot)
		return nil
	}
	for accountID, acct := range chCfg.Accounts {
		botToken := acct.BotToken
		if botToken == "" {
			botToken = chCfg.BotToken
		}
		sl, err := channels.NewSlack(botToken, chCfg.AppToken, accountID, mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, sl, hot)
	}
	return nil
}

func registerLINEChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	// LINE 行携带一个或多个以 bot userId 为键的 (channel_access_token, channel_secret) 对。
	// AccountConfig.BotToken 是通道访问令牌；AccountConfig.UserID 是通道密钥（用于入站 HMAC 验证 — 见 channels/line.go 字段映射注释）。
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		ln, err := channels.NewLINE(token, acct.UserID, accountID, mb)
		if err != nil {
			return err
		}
		register(chanMgr, ln, hot)
	}
	return nil
}

func registerFeishuChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	// 飞书是多账号的：一行携带一个或多个以 app_id 为键的 (app_id, app_secret, verification_token) 三元组。
	// 没有传统的单 bot 回退 — 每个账号的映射是连接处理程序生成的唯一形状。
	for accountID, acct := range chCfg.Accounts {
		secret := acct.BotToken
		if secret == "" {
			secret = chCfg.BotToken
		}
		// AccountConfig.UserID 携带验证令牌（见 channels/feishu.go 的字段映射说明）。
		// AccountConfig.EncryptKey 在用户在飞书控制台启用加密策略时设置；空值表示明文 webhook 体。
		// AccountConfig.UseLongConn 将适配器切换到出站 WebSocket 模式（无需公网 URL）；
		// 当为 true 时 verificationToken/encryptKey 字段未使用。
		lk, err := channels.NewFeishu(accountID, secret, acct.UserID, acct.EncryptKey, acct.UseLongConn, accountID, mb)
		if err != nil {
			return err
		}
		// 长连接打开飞书 WebSocket — 两个副本都会订阅 im.message.receive_v1，
		// bot 所有者会看到每条回复两次。Webhook 模式通过 HTTP 路由接收，
		// 在网关入口已经是幂等的（每个 HTTP POST 调用一次 HandleWebhook），
		// 因此跳过租约。
		if acct.UseLongConn {
			registerSingleton(chanMgr, lk, hot)
		} else {
			register(chanMgr, lk, hot)
		}
	}
	return nil
}

func registerWeChatChannels(rec store.ConfigRecord, chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, st store.Store, hot bool) error {
	// 微信在设计上是多账号的 — 每次二维码扫描都会生成一个新的 (botToken, ilink_user_id, baseURL) 三元组，
	// 以新的 accountID 为键。传统的"无 Accounts 映射 → 从顶层 BotToken 的单个 bot"形状不适用
	//（我们从来没有可用的顶层配置；每个账号的字段 BaseURL + UserID 是必需的）。因此跳过空 Accounts 回退。
	for accountID, acct := range chCfg.Accounts {
		token := acct.BotToken
		if token == "" {
			token = chCfg.BotToken
		}
		wc, err := channels.NewWeChat(token, acct.BaseURL, acct.UserID, accountID, mb)
		if err != nil {
			return err
		}
		// 在确认令牌过期时适配器退出；清理配置行，以便下次进程重启不会重新注册已知失效的 bot
		//（这会在启动时再次记录相同的警告）。用户必须通过仪表盘重新扫描二维码 —
		// 该流程从头重新创建 Accounts 条目。
		if st != nil {
			rowID := rec.ID
			wc.SetOnExpired(func(deadAccount string) {
				if err := purgeWeChatAccount(st, rowID, deadAccount); err != nil {
					slog.Warn("wechat token-expired cleanup failed",
						"account", deadAccount, "error", err)
				}
				chanMgr.Unregister("wechat", deadAccount)
			})
		}
		registerSingleton(chanMgr, wc, hot)
	}
	return nil
}

// purgeWeChatAccount 从配置行的 Accounts 映射中移除一个账号。
// 如果移除后行被留空，则整个行被删除。在适配器的轮询 goroutine 中运行，
// 因此 HTTP 请求 ctx 不可用 — 使用新的后台 ctx。
//
// 幂等的：GetConfig 查找返回 ErrNotFound 意味着行已经不存在
//（仪表盘断开连接，或兄弟账号的清理先清空了该行）— 那是成功，不是错误。
func purgeWeChatAccount(st store.Store, rowID, deadAccount string) error {
	ctx := context.Background()
	rec, err := st.GetConfig(ctx, rowID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if rec == nil {
		return nil
	}
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, mErr := json.Marshal(rec.Data); mErr == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	if _, ok := cc.Accounts[deadAccount]; !ok {
		return nil
	}
	delete(cc.Accounts, deadAccount)
	if len(cc.Accounts) == 0 {
		return st.DeleteConfig(ctx, rec.ID)
	}
	blob, mErr := json.Marshal(cc)
	if mErr != nil {
		return mErr
	}
	var data map[string]interface{}
	if mErr := json.Unmarshal(blob, &data); mErr != nil {
		return mErr
	}
	rec.Data = data
	return st.SaveConfig(ctx, rec)
}
