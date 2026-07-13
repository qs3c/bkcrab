package agent

import (
	"context"
	"testing"
	"time"
)

func TestStopSkillJobsCancelsAndPreventsLateLearnerWork(t *testing.T) {
	a := &Agent{}
	started := make(chan struct{})
	stopped := make(chan struct{})
	if ok := a.launchSkillJob(context.Background(), func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(stopped)
	}); !ok {
		t.Fatal("initial learner job was not launched")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("learner job did not start")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.stopSkillJobs(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("learner job did not observe cancellation")
	}
	if ok := a.launchSkillJob(context.Background(), func(context.Context) {}); ok {
		t.Fatal("learner job launched after agent shutdown")
	}
}
