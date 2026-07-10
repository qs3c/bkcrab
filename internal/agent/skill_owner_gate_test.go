package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
)

type skillGateCaptureProvider struct {
	mu             sync.Mutex
	firstChatTools []provider.Tool
	learnerCall    chan struct{}
}

func (p *skillGateCaptureProvider) Chat(_ context.Context, _ []provider.Message, toolDefs []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	p.mu.Lock()
	if p.firstChatTools == nil {
		p.firstChatTools = append([]provider.Tool(nil), toolDefs...)
	}
	p.mu.Unlock()
	if len(toolDefs) == 1 && toolDefs[0].Function.Name == "skill_manage" && p.learnerCall != nil {
		select {
		case p.learnerCall <- struct{}{}:
		default:
		}
	}
	return &provider.Response{Content: "ok"}, nil
}

func (p *skillGateCaptureProvider) ChatStream(_ context.Context, _ []provider.Message, toolDefs []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	p.mu.Lock()
	if p.firstChatTools == nil {
		p.firstChatTools = append([]provider.Tool(nil), toolDefs...)
	}
	p.mu.Unlock()
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Content: "ok", Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func (p *skillGateCaptureProvider) sawSkillManage() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, def := range p.firstChatTools {
		if def.Function.Name == "skill_manage" {
			return true
		}
	}
	return false
}

func (p *skillGateCaptureProvider) capturedTools() []provider.Tool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Tool(nil), p.firstChatTools...)
}

func newSkillGateAgent(t *testing.T, p provider.Provider) *Agent {
	t.Helper()
	home := t.TempDir()
	a := NewAgent(config.ResolvedAgent{
		ID: "agent-a", UserID: "owner-a", Home: home, Workspace: t.TempDir(),
		Model: "test/model", MaxTokens: 256, MaxToolIterations: 2,
	}, p, bus.New(), t.TempDir())
	a.skillsLearner = NewSkillsLearner(home, p, "test/model")
	a.skillsLearner.minToolCalls = 99
	a.registry.SetSkillManage(a.skillsLearner.Manager(), nil)
	return a
}

func TestSkillManageVisibilityMatchesOwnerAcrossLoopVariants(t *testing.T) {
	tests := []struct {
		name     string
		stream   bool
		userID   string
		peerKind string
		want     bool
	}{
		{name: "nonstream owner dm", userID: "owner-a", peerKind: "dm", want: true},
		{name: "nonstream guest dm", userID: "guest-a", peerKind: "dm"},
		{name: "nonstream owner group", userID: "owner-a", peerKind: "group"},
		{name: "stream owner dm", stream: true, userID: "owner-a", peerKind: "dm", want: true},
		{name: "stream guest dm", stream: true, userID: "guest-a", peerKind: "dm"},
		{name: "stream owner group", stream: true, userID: "owner-a", peerKind: "group"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &skillGateCaptureProvider{}
			a := newSkillGateAgent(t, p)
			msg := bus.InboundMessage{Channel: "web", ChatID: tt.name, UserID: tt.userID, PeerKind: tt.peerKind, Text: "hello"}
			if tt.stream {
				sr := a.HandleMessageStream(context.Background(), msg)
				for {
					if _, ok := sr.Next(); !ok {
						break
					}
				}
			} else {
				_ = a.HandleMessage(context.Background(), msg)
			}
			if got := p.sawSkillManage(); got != tt.want {
				t.Fatalf("skill_manage visible = %v, want %v; tools=%+v owner=%q runtime=%q", got, tt.want, p.capturedTools(), a.agentOwnerUserID, a.ownerUserID)
			}
		})
	}
}

func TestLearnerOwnerTurnFailsClosed(t *testing.T) {
	a := &Agent{agentOwnerUserID: "owner-a"}
	tests := []struct {
		name       string
		msg        bus.InboundMessage
		chatterUID string
		want       bool
	}{
		{name: "owner dm", msg: bus.InboundMessage{UserID: "owner-a", PeerKind: "dm"}, chatterUID: "owner-a", want: true},
		{name: "guest", msg: bus.InboundMessage{UserID: "guest-a", PeerKind: "dm"}, chatterUID: "guest-a"},
		{name: "owner group", msg: bus.InboundMessage{UserID: "owner-a", PeerKind: "group"}, chatterUID: "owner-a"},
		{name: "synthetic", msg: bus.InboundMessage{UserID: "owner-a", PeerKind: "dm", Source: bus.SourceCron}, chatterUID: "owner-a"},
		{name: "missing explicit chatter", msg: bus.InboundMessage{PeerKind: "dm"}, chatterUID: "owner-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.isLearnerOwnerTurn(tt.msg, tt.chatterUID); got != tt.want {
				t.Fatalf("isLearnerOwnerTurn = %v, want %v", got, tt.want)
			}
		})
	}
	a.agentOwnerUserID = ""
	if a.isLearnerOwnerTurn(bus.InboundMessage{UserID: "owner-a", PeerKind: "dm"}, "owner-a") {
		t.Fatal("legacy empty agents.user_id must fail closed")
	}
}

func TestRunPostTurnStartsLearnerOnlyForOwner(t *testing.T) {
	for _, tt := range []struct {
		name   string
		userID string
		want   bool
	}{
		{name: "owner", userID: "owner-a", want: true},
		{name: "guest", userID: "guest-a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &skillGateCaptureProvider{learnerCall: make(chan struct{}, 1)}
			a := newSkillGateAgent(t, p)
			a.skillsLearner.minToolCalls = 1
			msg := bus.InboundMessage{Channel: "web", ChatID: tt.name, UserID: tt.userID, PeerKind: "dm"}
			a.runPostTurn(context.Background(), msg, []provider.Message{{Role: "user", Content: "material"}}, 1, a.memory.WithUserID(tt.userID), nil)
			if tt.want {
				select {
				case <-p.learnerCall:
				case <-time.After(time.Second):
					t.Fatal("owner turn did not start learner")
				}
				return
			}
			select {
			case <-p.learnerCall:
				t.Fatal("guest turn started learner")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}
