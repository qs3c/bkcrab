package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type loadRecord struct {
	agentID       string
	slug          string
	invokedByUser bool
	halfLifeLoads int
	explicitGain  int
	diskHash      string
}

type fakeLoadRecorder struct {
	ch  chan loadRecord
	err error
}

func (f *fakeLoadRecorder) RecordSkillLoad(ctx context.Context, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*store.SkillUsageRow, error) {
	f.ch <- loadRecord{agentID: agentID, slug: slug, diskHash: diskHash, invokedByUser: invokedByUser, halfLifeLoads: halfLifeLoads, explicitGain: explicitGain}
	return nil, f.err
}

type blockingLoadRecorder struct {
	entered chan struct{}
	release chan struct{}
}

type deletingLoadRecorder struct {
	records int
}

func (f *deletingLoadRecorder) RecordSkillLoad(context.Context, string, string, string, bool, int, int) (*store.SkillUsageRow, error) {
	f.records++
	return nil, nil
}

func (f *deletingLoadRecorder) IsAgentDeleting(context.Context, string) (bool, error) {
	return true, nil
}

func (f *blockingLoadRecorder) RecordSkillLoad(ctx context.Context, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*store.SkillUsageRow, error) {
	close(f.entered)
	select {
	case <-f.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestLoadSkillRegisteredByDefaultAndLoadsFullContent(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "chart-maker")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: chart-maker
description: Build charts from tabular data.
---

Run {baseDir}/scripts/render.py with JSON input.`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkill(r, []string{filepath.Join(home, "skills")})

	fn := r.GetFunc("load_skill")
	if fn == nil {
		t.Fatal("load_skill was not registered")
	}
	rawArgs, err := json.Marshal(map[string]string{"name": "chart-maker"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fn(context.Background(), rawArgs)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "Run "+skillDir+"/scripts/render.py") {
		t.Fatalf("load_skill did not return full content with baseDir replaced:\n%s", got)
	}
	if !strings.Contains(got, "INTERNAL CONTEXT") {
		t.Fatalf("load_skill output missing internal wrapper:\n%s", got)
	}
}

func TestLoadSkillUsesDirectoryPrecedence(t *testing.T) {
	agentSkills := filepath.Join(t.TempDir(), "skills")
	userSkills := filepath.Join(t.TempDir(), "skills")
	for _, dir := range []string{agentSkills, userSkills} {
		if err := os.MkdirAll(filepath.Join(dir, "shared"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(userSkills, "shared", "SKILL.md"), []byte("user version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentSkills, "shared", "SKILL.md"), []byte("agent version"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkill(r, []string{agentSkills, userSkills})
	rawArgs, err := json.Marshal(map[string]string{"name": "shared"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetFunc("load_skill")(context.Background(), rawArgs)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "agent version") {
		t.Fatalf("load_skill did not use first matching directory:\n%s", got)
	}
	if strings.Contains(got, "user version") {
		t.Fatalf("load_skill should not include lower-priority skill:\n%s", got)
	}
}

func TestLoadSkillWithLedgerRecordsSuccessfulLoad(t *testing.T) {
	agentSkills := filepath.Join(t.TempDir(), "skills")
	skillDir := filepath.Join(agentSkills, "shared")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "ledger version"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	rec := &fakeLoadRecorder{ch: make(chan loadRecord, 1)}
	RegisterLoadSkillWithLedger(r, []string{agentSkills}, agentSkills, rec, "agentA", 32, 3)
	rawArgs, err := json.Marshal(map[string]any{"name": "shared", "invoked_by_user": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetFunc("load_skill")(context.Background(), rawArgs); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rec.ch:
		if got.agentID != "agentA" || got.slug != "shared" || !got.invokedByUser || got.halfLifeLoads != 32 || got.explicitGain != 3 {
			t.Fatalf("unexpected ledger record: %+v", got)
		}
		if got.diskHash != store.HashSkillContent(body) {
			t.Fatalf("diskHash=%s want %s", got.diskHash, store.HashSkillContent(body))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ledger record")
	}
}

func TestLoadSkillWaitsForLedgerBeforeReturning(t *testing.T) {
	learnerSkills := filepath.Join(t.TempDir(), "learner-skills")
	skillDir := filepath.Join(learnerSkills, "shared")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("learner version"), 0o644); err != nil {
		t.Fatal(err)
	}

	recorder := &blockingLoadRecorder{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkillWithLedger(r, []string{learnerSkills}, learnerSkills, recorder, "agentA", 32, 3)
	rawArgs, err := json.Marshal(map[string]string{"name": "shared"})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		content string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		content, callErr := r.GetFunc("load_skill")(context.Background(), rawArgs)
		done <- result{content: content, err: callErr}
	}()

	select {
	case <-recorder.entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ledger call")
	}
	select {
	case got := <-done:
		t.Fatalf("load_skill returned before ledger call completed: %+v", got)
	default:
	}
	close(recorder.release)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !strings.Contains(got.content, "learner version") {
			t.Fatalf("unexpected skill content: %s", got.content)
		}
	case <-time.After(time.Second):
		t.Fatal("load_skill did not return after ledger call completed")
	}
}

func TestLoadSkillLedgerFailureStillReturnsContent(t *testing.T) {
	learnerSkills := filepath.Join(t.TempDir(), "learner-skills")
	skillDir := filepath.Join(learnerSkills, "shared")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("learner version"), 0o644); err != nil {
		t.Fatal(err)
	}

	recorder := &fakeLoadRecorder{ch: make(chan loadRecord, 1), err: errors.New("ledger unavailable")}
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkillWithLedger(r, []string{learnerSkills}, learnerSkills, recorder, "agentA", 32, 3)
	rawArgs, err := json.Marshal(map[string]string{"name": "shared"})
	if err != nil {
		t.Fatal(err)
	}
	content, err := r.GetFunc("load_skill")(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("ledger failure blocked skill content: %v", err)
	}
	if !strings.Contains(content, "learner version") {
		t.Fatalf("unexpected skill content: %s", content)
	}
	select {
	case <-recorder.ch:
	default:
		t.Fatal("ledger call had not completed when load_skill returned")
	}
	if !strings.Contains(logs.String(), "skill load ledger record failed") || !strings.Contains(logs.String(), "ledger unavailable") {
		t.Fatalf("ledger failure warning missing from logs: %s", logs.String())
	}
}

func TestLoadSkillRefusesStaleLearnerAssetAfterAgentDeletionMarker(t *testing.T) {
	learnerSkills := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	skillDir := filepath.Join(learnerSkills, "stale")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("stale learner content"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := &deletingLoadRecorder{}
	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkillWithLedger(r, []string{learnerSkills}, learnerSkills, recorder, "deleted-agent", 32, 3)
	rawArgs, _ := json.Marshal(map[string]string{"name": "stale"})
	if _, err := r.GetFunc("load_skill")(context.Background(), rawArgs); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("stale learner load err=%v", err)
	}
	if recorder.records != 0 {
		t.Fatalf("deleted agent learner load was recorded %d times", recorder.records)
	}
}

func TestLoadSkillRefreshesRemoteDeletionBeforeReadingStalePodCache(t *testing.T) {
	ctx := context.Background()
	db, err := store.NewDBStore("sqlite", "file:load-skill-stale-pod?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const (
		agentID = "shared-agent"
		slug    = "deleted-workflow"
		content = "---\nname: Deleted\ndescription: deleted workflow\n---\nstale steps"
	)
	learnerRoot := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	skillDir := filepath.Join(learnerRoot, slug)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := workspace.NewLocalFS(t.TempDir())
	if err := skills.SyncLearnerSkillContent(ctx, remote, agentID, slug, content); err != nil {
		t.Fatal(err)
	}
	if err := skills.DeleteLearnerSkillUp(ctx, remote, agentID, slug); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	RegisterLoadSkillWithPolicyAndWorkspace(r, []string{learnerRoot}, learnerRoot, db, agentID, remote, 32, 3, true)
	rawArgs, _ := json.Marshal(map[string]string{"name": slug})
	if _, err := r.GetFunc("load_skill")(ctx, rawArgs); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("stale learner load err=%v", err)
	}
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("remote deletion was not pruned before load: %v", err)
	}
	rows, err := db.ListSkillUsage(ctx, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("stale load recreated deleted lifecycle ledger: %+v", rows)
	}
}

func TestLoadSkillManualShadowDoesNotRefreshLearnerLedger(t *testing.T) {
	manualSkills := filepath.Join(t.TempDir(), "skills")
	learnerSkills := filepath.Join(t.TempDir(), "learner-skills")
	for _, dir := range []string{manualSkills, learnerSkills} {
		if err := os.MkdirAll(filepath.Join(dir, "shared"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(manualSkills, "shared", "SKILL.md"), []byte("manual version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(learnerSkills, "shared", "SKILL.md"), []byte("learner version"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(t.TempDir(), t.TempDir())
	rec := &fakeLoadRecorder{ch: make(chan loadRecord, 1)}
	RegisterLoadSkillWithLedger(r, []string{manualSkills, learnerSkills}, learnerSkills, rec, "agentA", 32, 3)
	rawArgs, err := json.Marshal(map[string]any{"name": "shared", "invoked_by_user": true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetFunc("load_skill")(context.Background(), rawArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "manual version") || strings.Contains(got, "learner version") {
		t.Fatalf("load_skill precedence result:\n%s", got)
	}
	select {
	case record := <-rec.ch:
		t.Fatalf("manual shadow unexpectedly refreshed learner ledger: %+v", record)
	case <-time.After(100 * time.Millisecond):
	}
}
