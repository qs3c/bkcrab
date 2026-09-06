package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/session"
)

func TestTurnSteerLifecycle(t *testing.T) {
	a := &Agent{sessions: session.NewManager(t.TempDir())}
	if got := a.SteerWeb("A", "idle"); got.Buffered || got.State != TurnIdle {
		t.Fatalf("idle: %+v", got)
	}
	ctx, finish, err := a.ReserveWebTurn(context.Background(), "A", "project")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if got := a.SteerWeb("A", "startup"); !got.Buffered || got.State != TurnStarting {
		t.Fatalf("startup steer: %+v", got)
	}
	if got := a.SteerWeb("B", "unrelated"); got.Buffered || got.State != TurnIdle {
		t.Fatalf("other session: %+v", got)
	}
	sess := a.sessions.Get("web", "", "A", "project")
	a.bindTurnSession(ctx, sess)
	if a.WebTurnState("A") != TurnRunning {
		t.Fatal("not running")
	}
	if got := a.drainTurnSteer(ctx, true); len(got) != 1 || got[0].Content != "startup" {
		t.Fatalf("startup drain: %+v", got)
	}
	if a.WebTurnState("A") != TurnRunning {
		t.Fatal("nonempty final drain must keep intake open")
	}
	if got := a.drainTurnSteer(ctx, true); len(got) != 0 {
		t.Fatal(got)
	}
	if got := a.SteerWeb("A", "too late"); got.Buffered || got.State != TurnFinishing {
		t.Fatalf("closing steer must not report idle: %+v", got)
	}
	if !a.WebTurnActive("A") {
		t.Fatal("finishing must still occupy the slot")
	}
	if _, _, err := a.ReserveWebTurn(context.Background(), "A", ""); err != ErrTurnActive {
		t.Fatalf("finishing reservation: %v", err)
	}
	finish()
	if a.WebTurnState("A") != TurnIdle {
		t.Fatal("not released")
	}
}

func TestTurnSteerCleanupPersistsAcceptedMessages(t *testing.T) {
	for _, bind := range []bool{false, true} {
		for _, stop := range []string{"finish", "stop", "parent-cancel"} {
			t.Run(fmt.Sprintf("bound=%t/%s", bind, stop), func(t *testing.T) {
				a := &Agent{sessions: session.NewManager(t.TempDir())}
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx, finish, err := a.ReserveWebTurn(parent, "A", "project")
				if err != nil {
					t.Fatal(err)
				}
				defer finish()
				if bind {
					a.bindTurnSession(ctx, a.sessions.Get("web", "", "A", "project"))
				}
				if !a.SteerWeb("A", "accepted").Buffered {
					t.Fatal("steer rejected")
				}
				if stop == "stop" {
					a.StopWebTurn("A")
				}
				if stop == "parent-cancel" {
					cancel()
				}
				if stop != "finish" {
					if got := a.SteerWeb("A", "rejected"); got.Buffered || got.State != TurnStopping {
						t.Fatalf("stopping steer: %+v", got)
					}
					if !a.WebTurnActive("A") {
						t.Fatal("stop released before cleanup")
					}
				}
				finish()
				finish()
				msgs := a.sessions.Get("web", "", "A", "project").GetMessages()
				if len(msgs) != 1 || msgs[0].Content != "accepted" {
					t.Fatalf("history: %+v", msgs)
				}
				if a.WebTurnActive("A") {
					t.Fatal("cleanup did not release")
				}
			})
		}
	}
}

// Multiple producers racing a consumer must deliver each accepted instruction
// once. No Session active flag or independent inbox participates in this test.
func TestTurnSteerConcurrentPushDrain(t *testing.T) {
	a := &Agent{sessions: session.NewManager(t.TempDir())}
	ctx, finish, err := a.ReserveWebTurn(context.Background(), "A", "")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	const producers, perProducer = 8, 200
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				if !a.SteerWeb("A", fmt.Sprintf("%d/%d", p, i)).Buffered {
					t.Error("unexpected rejection")
				}
			}
		}(p)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	seen := make(map[string]bool)
	collect := func() {
		for _, msg := range a.drainTurnSteer(ctx, false) {
			if seen[msg.Content] {
				t.Errorf("duplicate %s", msg.Content)
			}
			seen[msg.Content] = true
		}
	}
	for {
		collect()
		select {
		case <-done:
			collect()
			if len(seen) != producers*perProducer {
				t.Fatalf("delivered %d", len(seen))
			}
			return
		default:
		}
	}
}

func TestTurnSteerRacesFinishWithoutLoss(t *testing.T) {
	a := &Agent{sessions: session.NewManager(t.TempDir())}
	_, finish, err := a.ReserveWebTurn(context.Background(), "A", "")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	start := make(chan struct{})
	accepted := make(chan string, 64)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			text := fmt.Sprint(i)
			if a.SteerWeb("A", text).Buffered {
				accepted <- text
			}
		}(i)
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; finish() }()
	close(start)
	wg.Wait()
	close(accepted)
	want := map[string]bool{}
	for text := range accepted {
		want[text] = true
	}
	got := map[string]bool{}
	for _, msg := range a.sessions.Get("web", "", "A", "").GetMessages() {
		if got[msg.Content] {
			t.Fatalf("duplicate %s", msg.Content)
		}
		got[msg.Content] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted=%v accepted=%v", got, want)
	}
}

type heldSteerPersistence struct {
	session.SessionStore
	started   chan struct{}
	release   chan struct{}
	projectID string
}

func (*heldSteerPersistence) ResolveActiveSessionKey(context.Context, string, string, string, string) (string, error) {
	return "A", nil
}
func (*heldSteerPersistence) GetSession(context.Context, string, string) ([]provider.Message, error) {
	return nil, nil
}
func (s *heldSteerPersistence) SaveSession(_ context.Context, _, _, _, _, _, projectID string, _ []provider.Message) error {
	s.projectID = projectID
	return nil
}
func (s *heldSteerPersistence) AppendMessage(context.Context, string, string, provider.Message) error {
	close(s.started)
	<-s.release
	return nil
}

func TestStartupSteerCleanupKeepsReservedProject(t *testing.T) {
	st := &heldSteerPersistence{started: make(chan struct{}), release: make(chan struct{})}
	close(st.release)
	a := &Agent{sessions: session.NewManagerWithStoreForUser(t.TempDir(), st, "user", "agent")}
	key := turnAddress{"web", "", "A"}
	_, finish, err := a.acquireTurn(context.Background(), key, "original-project", false)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if !a.pushTurnSteer(key, provider.Message{Role: "user", Content: "startup"}).Buffered {
		t.Fatal("steer rejected")
	}
	// Exit before binding history, as happens when stopped during initialization.
	finish()
	if st.projectID != "original-project" {
		t.Fatalf("persisted project: %q", st.projectID)
	}
}

func TestTurnKeepsSlotWhilePersistingSteerWithoutBlockingOtherSessions(t *testing.T) {
	a := &Agent{sessions: session.NewManager(t.TempDir())}
	ctx, finish, err := a.ReserveWebTurn(context.Background(), "A", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &heldSteerPersistence{started: make(chan struct{}), release: make(chan struct{})}
	persistence := session.NewManagerWithStoreForUser(t.TempDir(), st, "user", "agent")
	a.bindTurnSession(ctx, persistence.Get("web", "", "A", ""))
	if !a.SteerWeb("A", "saved during cleanup").Buffered {
		t.Fatal("steer rejected")
	}
	done := make(chan struct{})
	go func() { defer close(done); finish() }()
	defer func() { close(st.release); <-done }()
	select {
	case <-st.started:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not start persistence")
	}
	// Run the checks with a deadline: holding turns.mu across persistence would
	// otherwise deadlock the test as well as all unrelated sessions in production.
	checked := make(chan struct{})
	go func() {
		defer close(checked)
		if got := a.SteerWeb("A", "late"); got.Buffered || got.State != TurnFinishing {
			t.Errorf("closing steer: %+v", got)
		}
		if _, _, err := a.ReserveWebTurn(context.Background(), "A", ""); err != ErrTurnActive {
			t.Errorf("A: %v", err)
		}
		_, finishB, err := a.ReserveWebTurn(context.Background(), "B", "")
		if err != nil {
			t.Error(err)
			return
		}
		finishB()
	}()
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("persistence blocked unrelated session admission")
	}
}

type steerCheckpointProvider struct{ onCall func([]provider.Message) }

func (p *steerCheckpointProvider) Chat(_ context.Context, msgs []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	p.onCall(msgs)
	return &provider.Response{Content: "answer"}, nil
}

func (p *steerCheckpointProvider) ChatStream(_ context.Context, msgs []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	p.onCall(msgs)
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Content: "answer", Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func TestTurnSteerCheckpointsAcrossEntryPoints(t *testing.T) {
	for _, mode := range []string{"react", "stream", "plan", "react-cap", "stream-cap"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			p := &steerCheckpointProvider{}
			iterations := 3
			if mode == "react-cap" || mode == "stream-cap" {
				iterations = 0
			}
			a := NewAgent(config.ResolvedAgent{ID: "a", Home: filepath.Join(root, "home"), Workspace: filepath.Join(root, "workspace"), Model: "fake/model", MaxToolIterations: iterations}, p, nil, root)
			ctx, finish, err := a.ReserveWebTurn(context.Background(), "A", "")
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			if got := a.SteerWeb("A", "startup"); !got.Buffered || got.State != TurnStarting {
				t.Fatalf("startup: %+v", got)
			}
			calls := 0
			p.onCall = func(msgs []provider.Message) {
				calls++
				contents := []string{}
				for _, msg := range msgs {
					if msg.Role == "user" {
						contents = append(contents, msg.Content)
					}
				}
				want := []string{"original", "startup"}
				if calls > 1 {
					want = append(want, "during-model")
				}
				if !reflect.DeepEqual(contents, want) {
					t.Errorf("call %d: user messages=%v want=%v", calls, contents, want)
				}
				if calls == 1 && !a.SteerWeb("A", "during-model").Buffered {
					t.Error("running steer rejected")
				}
			}
			msg := bus.InboundMessage{Channel: "web", ChatID: "A", Text: "original"}
			if mode == "plan" {
				msg.Params = map[string]any{"planMode": true}
			}
			if mode == "stream" || mode == "stream-cap" {
				reader := a.HandleMessageStream(ctx, msg)
				for {
					if _, ok := reader.Next(); !ok {
						break
					}
				}
			} else {
				a.HandleMessage(ctx, msg)
			}
			finish()
			wantCalls := 1
			if mode == "react" || mode == "stream" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
			seen := map[string]int{}
			for _, m := range a.sessions.Get("web", "", "A", "").GetMessages() {
				if m.Role == "user" {
					seen[m.Content]++
				}
			}
			if !reflect.DeepEqual(seen, map[string]int{"original": 1, "startup": 1, "during-model": 1}) {
				t.Fatalf("history: %v", seen)
			}
		})
	}
}
