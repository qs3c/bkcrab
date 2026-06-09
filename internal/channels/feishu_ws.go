package channels

import (
	"context"
	"log/slog"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// startLongConn runs the Feishu WebSocket client until ctx is done or
// the SDK returns a fatal error. Subscribes to im.message.receive_v1
// and reuses the existing dispatchInbound() path by translating the
// SDK's typed event back into our internal feishuMessageEvent shape —
// the rest of the pipeline (peer-kind detection, dedup ID, content
// JSON unwrap) is identical to the webhook path.
//
// We intentionally do NOT use the SDK's auth/send code; outbound
// messages still go through Feishu.SendMessage / fetchBotInfo (own
// tenant_access_token cache). The SDK is here purely as a transport
// for inbound events because the long-conn protocol is protobuf-
// framed and not worth hand-rolling (see feishu.go top-of-file note).
func (l *Feishu) startLongConn(ctx context.Context) error {
	// NewEventDispatcher's first two args (verificationToken,
	// encryptKey) are HTTP-mode concerns — the WS transport carries
	// no signature/encryption, so empty strings are correct.
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
	// SDK's Start blocks; it watches ctx and returns on cancel.
	return cli.Start(ctx)
}

// sdkEventToInternal converts a larkim.P2MessageReceiveV1 into the
// feishuMessageEvent shape dispatchInbound expects. SDK fields are
// pointer-strings; nil is normalized to "" via deref.
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
