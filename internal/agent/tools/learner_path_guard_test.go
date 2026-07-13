package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/skills"
)

func TestGenericToolsCannotBypassLearnerSkillManager(t *testing.T) {
	systemRoot := t.TempDir()
	userRoot := t.TempDir()
	r := NewRegistry(systemRoot, userRoot)
	absoluteLearner := filepath.Join(systemRoot, skills.LearnerSkillsDirName, "private", "SKILL.md")

	cases := []struct {
		tool string
		args any
	}{
		{"read_file", map[string]any{"path": absoluteLearner}},
		{"write_file", map[string]any{"path": filepath.Join("..", skills.LearnerSkillsDirName, "private", "SKILL.md"), "content": "bypass"}},
		{"edit_file", map[string]any{"path": absoluteLearner, "old_string": "a", "new_string": "b"}},
		{"list_dir", map[string]any{"path": filepath.Dir(absoluteLearner)}},
		{"apply_patch", map[string]any{"input": "*** Begin Patch\n*** Add File: learner-skills/private/SKILL.md\n+bypass\n*** End Patch"}},
		{"exec", map[string]any{"command": "echo bypass > " + filepath.ToSlash(absoluteLearner)}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Execute(context.Background(), tc.tool, string(raw))
			if err == nil || !strings.Contains(err.Error(), "protected runtime-managed asset namespace") {
				t.Fatalf("%s bypass err=%v", tc.tool, err)
			}
		})
	}
}

func TestFileToolsRejectSymlinkAliasIntoLearnerNamespace(t *testing.T) {
	systemRoot := t.TempDir()
	userRoot := t.TempDir()
	learnerRoot := filepath.Join(systemRoot, skills.LearnerSkillsDirName)
	if err := os.MkdirAll(filepath.Join(learnerRoot, "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(userRoot, "innocent-alias")
	if err := os.Symlink(learnerRoot, alias); err != nil {
		t.Skipf("symlink unavailable in test environment: %v", err)
	}
	r := NewRegistry(systemRoot, userRoot)
	raw, _ := json.Marshal(map[string]any{"path": "innocent-alias/private/SKILL.md", "content": "bypass"})
	if _, err := r.Execute(context.Background(), "write_file", string(raw)); err == nil || !strings.Contains(err.Error(), "protected runtime-managed asset namespace") {
		t.Fatalf("symlink alias bypass err=%v", err)
	}
}
