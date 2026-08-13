package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

func TestRAGFairQueueCapacityFencePinsAndReleasesResourceLock(t *testing.T) {
	state := &fairQueueFenceDriverState{
		connIDs: []int64{7, 7, 7}, getResult: int64(1), releaseResult: int64(1),
	}
	st, writer := newFairQueueFenceTestStore(t, state)
	var callbackCalled bool
	err := st.withRAGFairQueueCapacityLock(
		context.Background(), writer, time.Second,
		func(_ ragFairQueueCapacitySession) error {
			callbackCalled = true
			return nil
		},
	)
	if err != nil {
		t.Fatalf("capacity fence: %v", err)
	}
	if !callbackCalled {
		t.Fatal("capacity callback was not called")
	}
	get, release, closed := state.counts()
	if get != 1 || release != 1 || closed != 0 {
		t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
	}
}

func TestRAGFairQueueCapacityFenceFailsClosedAndDiscardsUnsafeSession(t *testing.T) {
	tests := []struct {
		name          string
		state         *fairQueueFenceDriverState
		wantLockError bool
		wantWriterErr bool
	}{
		{
			name: "null acquire result",
			state: &fairQueueFenceDriverState{
				connIDs: []int64{7, 7}, getResult: nil, releaseResult: int64(1),
			},
			wantLockError: true,
		},
		{
			name: "release reports not owner",
			state: &fairQueueFenceDriverState{
				connIDs: []int64{7, 7, 7}, getResult: int64(1), releaseResult: driver.Value(int64(0)),
			},
		},
		{
			name: "session switches after acquire",
			state: &fairQueueFenceDriverState{
				connIDs: []int64{7, 8, 8}, getResult: int64(1), releaseResult: int64(1),
			},
			wantWriterErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, writer := newFairQueueFenceTestStore(t, tt.state)
			callbackCalled := false
			err := st.withRAGFairQueueCapacityLock(
				context.Background(), writer, time.Second,
				func(_ ragFairQueueCapacitySession) error {
					callbackCalled = true
					return nil
				},
			)
			if !errors.Is(err, ErrFairQueueUnsafeConnection) &&
				!errors.Is(err, ErrFairQueueWriterMismatch) {
				t.Fatalf("capacity fence error=%v", err)
			}
			if tt.wantWriterErr && !errors.Is(err, ErrFairQueueWriterMismatch) {
				t.Fatalf("writer mismatch error=%v", err)
			}
			if tt.wantLockError && !errors.Is(err, ErrRAGFairQueueCapacityLockUnavailable) {
				t.Fatalf("capacity lock error=%v", err)
			}
			if tt.name != "release reports not owner" && callbackCalled {
				t.Fatal("unsafe session reached callback")
			}
			_, _, closed := tt.state.counts()
			if closed != 1 {
				t.Fatalf("physical closes=%d, want 1", closed)
			}
		})
	}
}
