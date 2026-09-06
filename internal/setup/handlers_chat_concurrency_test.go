package setup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/api"
	"github.com/qs3c/bkcrab/internal/provider"
)

func TestChatSteerUsesReservationState(t *testing.T) {
	ctx := context.Background()
	s, resolver, _, user := newAuthTestServer(t, ctx)
	manager := newChatHistoryTestManager(t, user.ID)
	s.SetUserResolver(&chatHistoryResolver{spaces: map[string]*api.UserSpaceView{
		user.ID: {UserID: user.ID, Agents: manager},
	}})
	ag := manager.AgentByID("ctx-agent")
	_, finish, err := ag.ReserveWebTurn(ctx, "A", "original-project")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	steer := func(wantStatus int, wantState agent.TurnState) {
		t.Helper()
		r := authTestRequest(t, ctx, resolver, http.MethodPost, "/api/chat/steer", user.ID)
		r.Body = io.NopCloser(strings.NewReader(`{"agentId":"ctx-agent","sessionId":"A","projectId":"different-project","message":"adjust"}`))
		rr := httptest.NewRecorder()
		s.authMiddleware(s.handleChatSteer)(rr, r)
		var result agent.SteerResult
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if rr.Code != wantStatus || result.State != wantState || result.Buffered != (wantStatus == http.StatusOK) {
			t.Fatalf("status=%d result=%+v", rr.Code, result)
		}
	}
	// No HandleMessage/Session initialization is needed to accept steering.
	steer(http.StatusOK, agent.TurnStarting)
	ag.StopWebTurn("A")
	steer(http.StatusConflict, agent.TurnStopping)
	rr := httptest.NewRecorder()
	r := authTestRequest(t, ctx, resolver, http.MethodGet, "/api/chat/status?agentId=ctx-agent&sessionId=A", user.ID)
	s.authMiddleware(s.handleChatStatus)(rr, r)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"active":true`) || !strings.Contains(rr.Body.String(), `"state":"stopping"`) {
		t.Fatalf("status while stopping: %s", rr.Body.String())
	}
	finish()
	steer(http.StatusConflict, agent.TurnIdle)
	history, _ := json.Marshal(ag.WebChatHistory("A"))
	if strings.Count(string(history), `"content":"adjust"`) != 1 {
		t.Fatalf("accepted startup steer was lost or duplicated: %s", history)
	}
}

type blockingChatProvider struct {
	started chan context.Context
	release chan struct{}
}

func (p *blockingChatProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, errors.New("expected streaming provider call")
}

func (p *blockingChatProvider) ChatStream(ctx context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	p.started <- ctx
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.release:
	}
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Content: "completed after disconnect", Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func waitChatSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for chat")
	}
}

func TestChatDisconnectConcurrencyAndExplicitStop(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stop"}[stop], func(t *testing.T) {
			ctx := context.Background()
			s, resolver, _, user := newAuthTestServer(t, ctx)
			p := &blockingChatProvider{started: make(chan context.Context, 4), release: make(chan struct{})}
			defer close(p.release)
			manager := newChatHistoryTestManager(t, user.ID, p)
			s.SetUserResolver(&chatHistoryResolver{spaces: map[string]*api.UserSpaceView{
				user.ID: {UserID: user.ID, Agents: manager},
			}})
			ag := manager.AgentByID("ctx-agent")
			request := func(sid, path string) *http.Request {
				r := authTestRequest(t, ctx, resolver, http.MethodPost, path, user.ID)
				body, _ := json.Marshal(map[string]string{"agentId": "ctx-agent", "sessionId": sid, "message": "hello"})
				r.Body = io.NopCloser(strings.NewReader(string(body)))
				return r
			}
			start := func(sid string) (context.CancelFunc, <-chan struct{}, context.Context) {
				req := request(sid, "/api/chat/stream")
				clientCtx, disconnect := context.WithCancel(req.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					s.authMiddleware(s.handleChatStream)(httptest.NewRecorder(), req.WithContext(clientCtx))
				}()
				t.Cleanup(disconnect)
				select {
				case worker := <-p.started:
					return disconnect, done, worker
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not start")
					return nil, nil, nil
				}
			}
			disconnectA, doneA, workerA := start("A")
			disconnectA()
			waitChatSignal(t, doneA)
			if workerA.Err() != nil {
				t.Fatalf("disconnect canceled worker: %v", workerA.Err())
			}
			// Both endpoints must reject a duplicate before opening an SSE subscription.
			for _, endpoint := range []struct {
				path    string
				handler http.HandlerFunc
			}{{"/api/chat/stream", s.handleChatStream}, {"/api/chat", s.handleChat}} {
				rr := httptest.NewRecorder()
				s.authMiddleware(endpoint.handler)(rr, request("A", endpoint.path))
				if rr.Code != http.StatusConflict {
					t.Fatalf("%s duplicate status=%d: %s", endpoint.path, rr.Code, rr.Body.String())
				}
			}
			disconnectB, doneB, workerB := start("B")
			if workerA.Err() != nil || workerB.Err() != nil {
				t.Fatal("sessions failed to overlap")
			}
			if stop {
				rr := httptest.NewRecorder()
				s.authMiddleware(s.handleChatStop)(rr, request("A", "/api/chat/stop"))
				if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"stopped":true`) {
					t.Fatalf("stop: %s", rr.Body.String())
				}
				waitChatSignal(t, workerA.Done())
				if workerB.Err() != nil {
					t.Fatal("stop A canceled B")
				}
			}
			// Allow completion without closing the channel twice in cleanup.
			p.release <- struct{}{}
			if !stop {
				p.release <- struct{}{}
			}
			waitChatSignal(t, doneB)
			disconnectB()
			deadline := time.Now().Add(5 * time.Second)
			for ag.WebTurnActive("A") || ag.WebTurnActive("B") {
				if time.Now().After(deadline) {
					t.Fatal("worker did not release session")
				}
				time.Sleep(time.Millisecond)
			}
			if !stop {
				history, _ := json.Marshal(ag.WebChatHistory("A"))
				if !strings.Contains(string(history), "completed after disconnect") {
					t.Fatalf("reply not persisted: %s", history)
				}
			}
		})
	}
}
