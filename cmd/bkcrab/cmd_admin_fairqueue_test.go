package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/store"
)

type fakeFairQueueAdminRunner struct {
	contract, rabbit, rebind, rebuild int
	lastApply                         bool
}

func (f *fakeFairQueueAdminRunner) Contract(_ context.Context, apply, _ bool) (store.RAGFairQueueContractReport, error) {
	f.contract++
	f.lastApply = apply
	return store.RAGFairQueueContractReport{ExpandSchemaReady: true, Contracted: !apply, TaskCount: 3}, nil
}
func (f *fakeFairQueueAdminRunner) RabbitRepair(_ context.Context, resource string, apply bool, _ fairqueue.RabbitRepairAttestation) (fairqueue.RabbitRepairReport, error) {
	f.rabbit++
	f.lastApply = apply
	return fairqueue.RabbitRepairReport{Resource: resource, CandidateCount: 2, PagesScanned: 1}, nil
}
func (f *fakeFairQueueAdminRunner) WriterRebind(_ context.Context, resource, _ string, apply bool, _ fairqueue.WriterRebindAttestation) (fairqueue.WriterRebindReport, error) {
	f.rebind++
	f.lastApply = apply
	return fairqueue.WriterRebindReport{Resource: resource, ValidRunningCount: 0}, nil
}
func (f *fakeFairQueueAdminRunner) RedisForceRebuild(_ context.Context, resource string, apply bool, _ fairqueue.ForceRebuildAttestation) (fairqueue.ForceRebuildReport, error) {
	f.rebuild++
	f.lastApply = apply
	return fairqueue.ForceRebuildReport{Resource: resource, StandaloneRedis: true, CurrentWriterVerified: true, RabbitTruthSourceVerified: true, RebuildableKeyCount: 4, PagesScanned: 2}, nil
}

func executeFairQueueAdmin(t *testing.T, runner fairQueueAdminRunner, args ...string) (string, error) {
	t.Helper()
	cmd := adminFairQueueCmdWithRunner(runner)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return output.String(), err
}

func TestAdminFairQueueDefaultsToDryRunAndRejectsUnknownResourceBeforeRunner(t *testing.T) {
	runner := &fakeFairQueueAdminRunner{}
	output, err := executeFairQueueAdmin(t, runner, "contract-migrate")
	if err != nil || runner.contract != 1 || runner.lastApply || !strings.Contains(output, "mode=dry-run") {
		t.Fatalf("contract output=%q err=%v runner=%+v", output, err, runner)
	}
	for _, args := range [][]string{
		{"rabbit-disaster-repair"},
		{"rabbit-disaster-repair", "--resource", "unknown.resource"},
	} {
		if _, err := executeFairQueueAdmin(t, runner, args...); err == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
	if runner.rabbit != 0 || runner.rebuild != 0 {
		t.Fatalf("invalid resource reached runner: %+v", runner)
	}
}

func TestAdminFairQueueAcceptsImageResourceBeforeRunner(t *testing.T) {
	runner := &fakeFairQueueAdminRunner{}
	old := strings.Repeat("a", 64)
	for _, args := range [][]string{
		{"rabbit-disaster-repair", "--resource", "image.generate"},
		{"rebind-writer", "--resource", "image.generate", "--expected-old-writer-fingerprint", old},
		{"redis-force-rebuild", "--resource", "image.generate"},
	} {
		if _, err := executeFairQueueAdmin(t, runner, args...); err != nil {
			t.Fatalf("args %v rejected: %v", args, err)
		}
	}
	if runner.rabbit != 1 || runner.rebind != 1 || runner.rebuild != 1 {
		t.Fatalf("image resource did not reach runner: %+v", runner)
	}
}

func TestAdminFairQueueApplyRequiresEveryAttestationBeforeRunner(t *testing.T) {
	runner := &fakeFairQueueAdminRunner{}
	old := strings.Repeat("a", 64)
	tests := [][]string{
		{"contract-migrate", "--apply"},
		{"rabbit-disaster-repair", "--resource", "rag.index", "--apply", "--confirm-old-broker-isolated"},
		{"rebind-writer", "--resource", "rag.index", "--expected-old-writer-fingerprint", old, "--apply", "--confirm-old-writer-fenced", "--confirm-resource-runtimes-stopped"},
		{"redis-force-rebuild", "--resource", "rag.index", "--apply"},
	}
	for _, args := range tests {
		if _, err := executeFairQueueAdmin(t, runner, args...); err == nil {
			t.Fatalf("args %v accepted without complete attestation", args)
		}
	}
	if runner.contract+runner.rabbit+runner.rebind+runner.rebuild != 0 {
		t.Fatalf("missing confirmation reached runner: %+v", runner)
	}
}

func TestAdminFairQueueDryRunOutputIsAggregateOnly(t *testing.T) {
	runner := &fakeFairQueueAdminRunner{}
	output, err := executeFairQueueAdmin(t, runner, "redis-force-rebuild", "--resource", "rag.index")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"mode=dry-run", "resource=rag.index", "rebuildable=4", "pages=2"} {
		if !strings.Contains(output, required) {
			t.Fatalf("missing %q in %q", required, output)
		}
	}
	for _, forbidden := range []string{"dsn", "password", "operation_id", "owner", "epoch", "tenant", "task_id"} {
		if strings.Contains(strings.ToLower(output), forbidden) {
			t.Fatalf("output leaked forbidden field %q: %q", forbidden, output)
		}
	}
}

func TestAdminFairQueueNilRunnerFailsClosed(t *testing.T) {
	old := strings.Repeat("a", 64)
	for _, args := range [][]string{
		{"contract-migrate"},
		{"rabbit-disaster-repair", "--resource", "rag.index"},
		{"rebind-writer", "--resource", "rag.index", "--expected-old-writer-fingerprint", old},
		{"redis-force-rebuild", "--resource", "rag.index"},
	} {
		if _, err := executeFairQueueAdmin(t, nil, args...); err == nil || !strings.Contains(err.Error(), "runner is unavailable") {
			t.Fatalf("args %v error=%v, want unavailable runner", args, err)
		}
	}
}
