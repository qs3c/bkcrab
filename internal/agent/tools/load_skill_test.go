package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
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
	ch chan loadRecord
}

func (f *fakeLoadRecorder) RecordSkillLoad(ctx context.Context, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*store.SkillUsageRow, error) {
	f.ch <- loadRecord{agentID: agentID, slug: slug, diskHash: diskHash, invokedByUser: invokedByUser, halfLifeLoads: halfLifeLoads, explicitGain: explicitGain}
	return nil, nil
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
