package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/session"
)

func TestTurnControlIsolatesSessionsAndHoldsReservationAfterStop(t *testing.T) {
	a := &Agent{sessions: session.NewManager(t.TempDir())}
	ctxA, finishA, err := a.ReserveWebTurn(context.Background(), "A", "")
	if err != nil {
		t.Fatal(err)
	}
	defer finishA()
	ctxB, finishB, err := a.ReserveWebTurn(context.Background(), "B", "")
	if err != nil {
		t.Fatal(err)
	}
	defer finishB()
	if _, _, err := a.ReserveWebTurn(context.Background(), "A", ""); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("duplicate: %v", err)
	}
	// The worker uses the reservation instead of deadlocking on itself.
	_, nestedFinish, err := a.messageTurn(ctxA, bus.InboundMessage{Channel: "web", ChatID: "A"})
	if err != nil {
		t.Fatal(err)
	}
	nestedFinish()
	if !a.StopWebTurn("A") || ctxA.Err() != context.Canceled {
		t.Fatal("A not stopped")
	}
	if ctxB.Err() != nil {
		t.Fatal("stopping A canceled B")
	}
	if _, _, err := a.ReserveWebTurn(context.Background(), "A", ""); !errors.Is(err, ErrTurnActive) {
		t.Fatal("released before worker completed")
	}
	finishA()
	if a.WebTurnActive("A") {
		t.Fatal("A still active")
	}
	_, nextFinish, err := a.ReserveWebTurn(context.Background(), "A", "")
	if err != nil {
		t.Fatal(err)
	}
	defer nextFinish()
	finishA() // Old cleanup must not remove a newer reservation.
	if !a.WebTurnActive("A") {
		t.Fatal("old cleanup removed new turn")
	}
}

func TestTurnControlWaitIsCancelable(t *testing.T) {
	a := &Agent{}
	msg := bus.InboundMessage{Channel: "web", ChatID: "A"}
	_, finish, err := a.messageTurn(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := a.messageTurn(ctx, msg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait = %v", err)
	}
}

func TestMessageTurnProviderSessionPersistsAcrossTurns(t *testing.T) {
	a := &Agent{ownerUserID: "owner", name: "agent", sessions: session.NewManager(t.TempDir())}
	msg := bus.InboundMessage{Channel: "web", ChatID: "A"}
	first, finish, err := a.messageTurn(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	firstID := provider.SessionIDFromContext(first)
	finish()
	second, finish, err := a.messageTurn(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if firstID == "" || provider.SessionIDFromContext(second) != firstID {
		t.Fatal("conversation session changed between turns")
	}
	finish()
	msg.ChatID = "B"
	other, finish, err := a.messageTurn(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if provider.SessionIDFromContext(other) == firstID {
		t.Fatal("different chats shared a provider session")
	}
}

func TestEventHubConcurrentPublishAndUnsubscribe(t *testing.T) {
	hub := NewEventHub()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			hub.Publish("u", "a", "s", EventEnvelope{})
		}
	}()
	for i := 0; i < 1000; i++ {
		_, unsubscribe := hub.Subscribe("u", "a", "s")
		unsubscribe()
	}
	<-done
}

type heldFinalStreamProvider struct{ chunks chan provider.StreamChunk }

func (p *heldFinalStreamProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return &provider.Response{Content: "final"}, nil
}

func (p *heldFinalStreamProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return provider.NewStreamReader(p.chunks), nil
}

func TestStreamTurnReservationLastsUntilFinalPersistence(t *testing.T) {
	for _, maxIterations := range []int{0, 1} {
		t.Run(map[int]string{0: "iteration-cap", 1: "normal"}[maxIterations], func(t *testing.T) {
			root := t.TempDir()
			p := &heldFinalStreamProvider{chunks: make(chan provider.StreamChunk, 1)}
			a := NewAgent(config.ResolvedAgent{ID: "a", Home: filepath.Join(root, "home"), Workspace: filepath.Join(root, "workspace"), Model: "fake/model", MaxToolIterations: maxIterations}, p, nil, root)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reader := a.HandleMessageStream(ctx, bus.InboundMessage{Channel: "web", ChatID: "A", Text: "hello"})
			if _, _, err := a.ReserveWebTurn(ctx, "A", ""); !errors.Is(err, ErrTurnActive) {
				t.Fatalf("stream no longer owns turn: %v", err)
			}
			p.chunks <- provider.StreamChunk{Content: "persisted final", Done: true}
			close(p.chunks)
			for {
				if _, ok := reader.Next(); !ok {
					break
				}
			}
			if a.WebTurnActive("A") {
				t.Fatal("completed stream still owns turn")
			}
			messages := a.sessions.Get("web", "", "A", "").GetMessages()
			if len(messages) == 0 || !strings.Contains(messages[len(messages)-1].Content, "persisted final") {
				t.Fatalf("final message not persisted: %+v", messages)
			}
		})
	}
}
