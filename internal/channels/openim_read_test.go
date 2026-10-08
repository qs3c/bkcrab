package channels

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
)

func TestOpenIMReadExactMessage(t *testing.T) {
	searches, reads := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/get_admin_token":
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"token":"admin","expireTimeSeconds":3600}}`))
		case "/msg/search_msg":
			searches++
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request["sendID"] != "alice" || request["recvID"] != "bot" || request["sessionType"] != float64(1) {
				t.Error("unscoped search")
			}
			if searches == 1 {
				_, _ = w.Write([]byte(`{"errCode":0,"data":{"chatLogsNum":0,"chatLogs":[]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"chatLogsNum":4,"chatLogs":[
    {"chatLog":{"serverMsgID":"wanted","sendID":"other","recvID":"bot","sessionType":1,"seq":9}},
    {"chatLog":{"serverMsgID":"wanted","sendID":"alice","recvID":"bot","sessionType":1,"seq":"10"}},
    {"chatLog":{"serverMsgID":"future","sendID":"alice","recvID":"bot","sessionType":1,"seq":11}},
    {"chatLog":{"serverMsgID":"wanted","sendID":"alice","recvID":"other","sessionType":1,"seq":12}}
   ]}}`))
		case "/msg/mark_msgs_as_read":
			reads++
			var req struct {
				UserID, ConversationID string
				Seqs                   []int64
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.UserID != "bot" || req.ConversationID != "si_alice_bot" || !reflect.DeepEqual(req.Seqs, []int64{10}) {
				t.Errorf("wrong receipt: %+v", req)
			}
			_, _ = w.Write([]byte(`{"errCode":0,"data":{}}`))
		default:
			t.Errorf("unexpected API %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := openIMTestConfig()
	c.APIURL = server.URL
	o, _ := NewOpenIM(c, nil, nil)
	msg := bus.InboundMessage{Channel: "openim", AccountID: o.AccountID(), ChatID: o.chatID("dm", "alice"), PeerKind: "dm", MessageID: "wanted"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := o.MarkRead(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || searches != 2 {
		t.Fatalf("reads=%d searches=%d", reads, searches)
	}
	for _, change := range []func(*bus.InboundMessage){
		func(m *bus.InboundMessage) { m.PeerKind = "group" }, func(m *bus.InboundMessage) { m.Source = bus.SourceCron },
		func(m *bus.InboundMessage) { m.IsBotMessage = true }, func(m *bus.InboundMessage) { m.AccountID = "other" },
		func(m *bus.InboundMessage) { m.MessageID = "" }, func(m *bus.InboundMessage) { m.ChatID = o.chatID("dm", "bot") },
	} {
		copy := msg
		change(&copy)
		_ = o.MarkRead(ctx, copy)
	}
	if reads != 1 || searches != 2 {
		t.Fatal("invalid input reached API")
	}
	msg.MessageID = "missing"
	ctx2, cancel2 := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel2()
	if err := o.MarkRead(ctx2, msg); err == nil {
		t.Fatal("missing message should fail")
	}
	if reads != 1 {
		t.Fatal("missing message acknowledged another message")
	}
}

func TestOpenIMReadSearchRecentPages(t *testing.T) {
	var pages []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/get_admin_token" {
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"token":"admin","expireTimeSeconds":3600}}`))
			return
		}
		var req struct{ Pagination struct{ PageNumber int } }
		_ = json.NewDecoder(r.Body).Decode(&req)
		page := req.Pagination.PageNumber
		pages = append(pages, page)
		if page == 20 {
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"chatLogsNum":2050,"chatLogs":[{"chatLog":{"serverMsgID":"wanted","sendID":"alice","recvID":"bot","sessionType":1,"seq":4000}}]}}`))
		} else {
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"chatLogsNum":2050,"chatLogs":[]}}`))
		}
	}))
	defer server.Close()
	c := openIMTestConfig()
	c.APIURL = server.URL
	o, _ := NewOpenIM(c, nil, nil)
	seq, err := o.findReadSequence(context.Background(), "alice", "wanted")
	if err != nil || seq != 4000 || !reflect.DeepEqual(pages, []int{1, 21, 20}) {
		t.Fatalf("seq=%d pages=%v error=%v", seq, pages, err)
	}
	pages = nil
	seq, err = o.findReadSequence(context.Background(), "alice", "absent")
	if err != nil || seq != 0 || !reflect.DeepEqual(pages, []int{1, 21, 20, 19, 18, 17}) {
		t.Fatalf("unbounded lookup: seq=%d pages=%v error=%v", seq, pages, err)
	}
}
