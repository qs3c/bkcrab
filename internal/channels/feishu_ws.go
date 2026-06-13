package channels

import (
	"context"
	"log/slog"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// startLongConn 运行飞书 WebSocket 客户端直到 ctx 完成或 SDK 返回致命错误。
// 订阅 im.message.receive_v1 并通过将 SDK 的类型化事件转换回我们的内部
// feishuMessageEvent 形状来重用现有的 dispatchInbound() 路径——管道的其余
// 部分（对等类型检测、去重 ID、内容 JSON 解包）与 webhook 路径相同。
//
// 我们故意不使用 SDK 的认证/发送代码；出站消息仍然通过
// Feishu.SendMessage / fetchBotInfo（自己的 tenant_access_token 缓存）。
// SDK 仅作为入站事件的传输，因为长连接协议是 protobuf 帧格式，
// 不值得手写（见 feishu.go 文件顶部注释）。
func (l *Feishu) startLongConn(ctx context.Context) error {
	// NewEventDispatcher 的前两个参数（verificationToken、encryptKey）
	// 是 HTTP 模式关注点——WS 传输不携带签名/加密，因此空字符串是正确的。
	d := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			l.dispatchInbound(sdkEventToInternal(ev))
			return nil
		})

	cli := larkws.NewClient(l.appID, l.appSecret,
		larkws.WithEventHandler(d),
		larkws.WithAutoReconnect(true),
	)
	slog.Info("feishu long-connection starting", "account", l.accountID)
	// SDK 的 Start 阻塞；它监视 ctx 并在取消时返回。
	return cli.Start(ctx)
}

// sdkEventToInternal 将 larkim.P2MessageReceiveV1 转换为 dispatchInbound
// 期望的 feishuMessageEvent 形状。SDK 字段是指针字符串；nil 通过解引用
// 归一化为 ""。
func sdkEventToInternal(ev *larkim.P2MessageReceiveV1) feishuMessageEvent {
	var out feishuMessageEvent
	if ev == nil || ev.Event == nil {
		return out
	}
	if s := ev.Event.Sender; s != nil {
		out.Sender.SenderType = derefStr(s.SenderType)
		if id := s.SenderId; id != nil {
			out.Sender.SenderID.OpenID = derefStr(id.OpenId)
			out.Sender.SenderID.UserID = derefStr(id.UserId)
			out.Sender.SenderID.UnionID = derefStr(id.UnionId)
		}
	}
	if m := ev.Event.Message; m != nil {
		out.Message.MessageID = derefStr(m.MessageId)
		out.Message.RootID = derefStr(m.RootId)
		out.Message.ParentID = derefStr(m.ParentId)
		out.Message.CreateTime = derefStr(m.CreateTime)
		out.Message.ChatID = derefStr(m.ChatId)
		out.Message.ChatType = derefStr(m.ChatType)
		out.Message.MessageType = derefStr(m.MessageType)
		out.Message.Content = derefStr(m.Content)
	}
	return out
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
