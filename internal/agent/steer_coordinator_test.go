package agent

import (
	"context"
	"testing"

	"github.com/qs3c/bkclaw/internal/bus"
	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/session"
)

type recordingTurnCoordinator struct {
	pushOK      bool
	key         session.TurnKey
	msgs        []provider.Message
	endCalls    int
	endLeftover []provider.Message
}

func (r *recordingTurnCoordinator) BeginTurn(ctx context.Context, key session.TurnKey) (*session.TurnLease, bool, error) {
	return &session.TurnLease{Key: key}, true, nil
}

func (r *recordingTurnCoordinator) PushSteer(ctx context.Context, key session.TurnKey, msg provider.Message) (bool, error) {
	r.key = key
	r.msgs = append(r.msgs, msg)
	return r.pushOK, nil
}

func (r *recordingTurnCoordinator) DrainSteer(ctx context.Context, lease *session.TurnLease) ([]provider.Message, error) {
	return nil, nil
}

func (r *recordingTurnCoordinator) EndTurn(ctx context.Context, lease *session.TurnLease) ([]provider.Message, error) {
	r.endCalls++
	return r.endLeftover, nil
}

func (r *recordingTurnCoordinator) Close() error { return nil }

func TestSteerInboundUsesTurnCoordinator(t *testing.T) {
	coord := &recordingTurnCoordinator{pushOK: true}
	ag := &Agent{
		sessions: session.NewManager(t.TempDir()),
		turns:    coord,
	}

	ok := ag.SteerInbound(bus.InboundMessage{
		Channel:   "slack",
		AccountID: "workspace-1",
		ChatID:    "thread-1",
	}, "change direction")

	if !ok {
		t.Fatal("SteerInbound should return the coordinator decision")
	}
	if len(coord.msgs) != 1 {
		t.Fatalf("coordinator received %d steer messages, want 1", len(coord.msgs))
	}
	if coord.msgs[0].Role != "user" || coord.msgs[0].Content != "change direction" {
		t.Fatalf("coordinator message = %#v, want user steer content", coord.msgs[0])
	}
	if coord.key.SessionKey == "" {
		t.Fatal("coordinator should receive the resolved session key")
	}
}

func TestFlushLeftoverSteerEndsCoordinatorLease(t *testing.T) {
	coord := &recordingTurnCoordinator{
		endLeftover: []provider.Message{{Role: "user", Content: "late steer"}},
	}
	ag := &Agent{
		sessions: session.NewManager(t.TempDir()),
		turns:    coord,
	}
	sess := ag.sessions.Get("slack", "workspace-1", "thread-1", "")
	lease := &session.TurnLease{Key: sess.TurnKey()}

	ag.flushLeftoverSteer(context.Background(), sess, lease)

	if coord.endCalls != 1 {
		t.Fatalf("EndTurn called %d times, want 1", coord.endCalls)
	}
	if len(sess.Messages) != 1 || sess.Messages[0].Content != "late steer" {
		t.Fatalf("session messages = %#v, want leftover steer appended", sess.Messages)
	}
}
