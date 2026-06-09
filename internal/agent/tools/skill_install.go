package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/skills"
)

// RegisterSkillInstall wires the per-agent skill search and install tools
// into registry r.
//
// agentSkillsDir is the per-agent skills directory (conventionally
// <agentHome>/skills). Agent-initiated installs always land under that path —
// never in the global ~/.fastclaw/skills/ — so one agent can't alter another
// agent's capabilities just by chatting.
//
// onReload is called after a successful install so the owning agent can
// re-scan its skills dir and expose the new skill on the next turn without
// a restart. Pass nil to disable hot reload.
func RegisterSkillInstall(r *Registry, agentSkillsDir string, onReload func()) {
	r.Register(
		"search_skills",
		"Search for skills on skills.sh (primary registry) and clawhub.ai. Returns the top matches so you can pick one to install.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query (e.g. 'pdf', 'translation', 'web scraping')",
				},
			},
			"required": []string{"query"},
		},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			var params struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", err
			}
			if params.Query == "" {
				return "", fmt.Errorf("query is required")
			}

			var b strings.Builder
			sh, shErr := skills.SearchSkillsSh(params.Query)
			if shErr == nil && len(sh) > 0 {
				fmt.Fprintf(&b, "skills.sh (%d results):\n", len(sh))
				limit := 10
				if len(sh) < limit {
					limit = len(sh)
				}
				for _, r := range sh[:limit] {
					fmt.Fprintf(&b, "- %s (from %s, %d installs)\n", r.SkillID, r.Source, r.Installs)
				}
			} else if shErr != nil {
				fmt.Fprintf(&b, "skills.sh search error: %v\n", shErr)
			} else {
				b.WriteString("skills.sh: no matches\n")
			}
			return b.String(), nil
		},
	)

	r.Register(
		"install_skill",
		"Install a skill into THIS agent's private skills directory. Tries skills.sh first, then clawhub.ai. If neither has it, returns a not-found error — at that point ask the user whether to build a custom skill with the skill-creator skill instead of retrying. Installed skills are scoped to this agent only; they do not affect other agents.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Skill name/slug (what you'd see listed on skills.sh or clawhub). For GitHub installs, use 'owner/repo' in the `repo` field instead.",
				},
				"repo": map[string]interface{}{
					"type":        "string",
					"description": "Optional: GitHub 'owner/repo' to install from a specific repo instead of the public registries. When set, `name` is the skill folder inside the repo (omit for whole-repo skills).",
				},
			},
			"required": []string{"name"},
		},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			var params struct {
				Name string `json:"name"`
				Repo string `json:"repo"`
			}
			if err := json.Unmarshal(args, &params); err != nil {
				return "", err
			}
			if params.Name == "" && params.Repo == "" {
				return "", fmt.Errorf("name or repo is required")
			}
			if agentSkillsDir == "" {
				return "", fmt.Errorf("agent skills directory not configured")
			}

			var (
				result *skills.Result
				err    error
			)
			switch {
			case params.Repo != "":
				result, err = skills.InstallFromGitHubRepo(params.Repo, params.Name, agentSkillsDir)
			default:
				result, err = skills.InstallAuto(params.Name, agentSkillsDir)
			}
			if err != nil {
				return "", fmt.Errorf("%w — if the user still wants this capability, offer to build a custom skill using the skill-creator skill", err)
			}

			if onReload != nil {
				onReload()
			}
			msg := fmt.Sprintf("Installed %q from %s to %s (%d files).", result.Name, result.Source, result.InstalledAt, result.FilesWritten)
			if result.Version != "" {
				msg += fmt.Sprintf(" Version/ref: %s.", result.Version)
			}
			msg += " The skill is now available in this agent only; it will be picked up on the next turn."
			return msg, nil
		},
	)
}
