package channels

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
)

type openIMSearchResult struct {
	Total int `json:"chatLogsNum"`
	Logs  []struct {
		Revoked bool `json:"isRevoked"`
		Message struct {
			ServerID    string      `json:"serverMsgID"`
			ClientID    string      `json:"clientMsgID"`
			SendID      string      `json:"sendID"`
			RecvID      string      `json:"recvID"`
			SessionType int         `json:"sessionType"`
			Seq         json.Number `json:"seq"`
		} `json:"chatLog"`
	} `json:"chatLogs"`
}

// MarkRead is called at agent intake, never at webhook acknowledgement. The
// callback may precede Kafka persistence, so resolve the exact ID with a bounded
// retry. A failed lookup must never fall back to marking the conversation read.
func (o *OpenIM) MarkRead(ctx context.Context, msg bus.InboundMessage) error {
	if msg.Channel != o.Name() || msg.AccountID != o.AccountID() || msg.PeerKind != "dm" || msg.Source != bus.SourceUser || msg.IsBotMessage || msg.MessageID == "" {
		return nil
	}
	parts := strings.Split(msg.ChatID, ".")
	if len(parts) != 3 || parts[0] != o.AccountID() || parts[1] != "dm" {
		return errors.New("openim: invalid read conversation")
	}
	peer, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(peer) == 0 {
		return errors.New("openim: invalid read peer")
	}
	if string(peer) == o.cfg.BotUserID {
		return nil
	}
	ids := []string{string(peer), o.cfg.BotUserID}
	slices.Sort(ids)
	conversationID := "si_" + strings.Join(ids, "_")
	for attempt := 0; attempt < 4; attempt++ {
		seq, err := o.findReadSequence(ctx, string(peer), msg.MessageID)
		if err != nil {
			return err
		}
		if seq > 0 {
			return o.api(ctx, "/msg/mark_msgs_as_read", map[string]any{"userID": o.cfg.BotUserID, "conversationID": conversationID, "seqs": []int64{seq}}, nil)
		}
		if attempt < 3 && !sleepOrDone(ctx, 250*time.Millisecond*time.Duration(1<<attempt)) {
			return ctx.Err()
		}
	}
	return errors.New("openim: exact message not found within read lookup window")
}

func (o *OpenIM) findReadSequence(ctx context.Context, peer, id string) (int64, error) {
	// v3.8 search is oldest-first. Inspect the first page then at most five
	// recent pages, so a long conversation does not permanently hide new input.
	page, lastPage := 1, 1
	for inspected := 0; inspected < 6; inspected++ {
		var result openIMSearchResult
		err := o.api(ctx, "/msg/search_msg", map[string]any{"sendID": peer, "recvID": o.cfg.BotUserID, "sessionType": 1, "pagination": map[string]int{"pageNumber": page, "showNumber": 100}}, &result)
		if err != nil {
			return 0, err
		}
		for _, entry := range result.Logs {
			m := entry.Message
			if entry.Revoked || m.SendID != peer || m.RecvID != o.cfg.BotUserID || m.SessionType != 1 || (m.ServerID != id && m.ClientID != id) {
				continue
			}
			seq, err := m.Seq.Int64()
			if err == nil && seq > 0 {
				return seq, nil
			}
		}
		if inspected == 0 {
			lastPage = (result.Total + 99) / 100
			if lastPage <= 1 {
				break
			}
			page = lastPage
		} else {
			page--
			if page <= 1 {
				break
			}
		}
	}
	return 0, nil
}
