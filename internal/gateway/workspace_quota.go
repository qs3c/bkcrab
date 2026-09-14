package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/qs3c/bkcrab/internal/sandbox"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

// Count per-user skill blobs together with all that user's agent workspaces.
// PublishedStore wraps this guard so publishing cannot bypass the quota.
func withWorkspaceQuota(st store.Store, ws workspace.Store, limits sandbox.Limits) workspace.Store {
	if limits.WorkspaceBytes > 0 {
		ws = workspace.NewQuotaStore(ws, limits.WorkspaceBytes, limits.WorkspaceFiles,
			func(ctx context.Context, id string) (string, error) {
				if id == skills.GlobalSkillOwner {
					return skills.GlobalSkillOwner, nil
				}
				if user, ok := strings.CutPrefix(id, "_user_"); ok && user != "" {
					return user, nil
				}
				a, err := st.GetAgent(ctx, id)
				if err != nil {
					return "", err
				}
				if a == nil || a.UserID == "" {
					return "", fmt.Errorf("workspace agent has no owner")
				}
				return a.UserID, nil
			}, func(ctx context.Context, user string) ([]string, error) {
				if user == skills.GlobalSkillOwner {
					return []string{skills.GlobalSkillOwner}, nil
				}
				rows, err := st.ListAgents(ctx, user)
				if err != nil {
					return nil, err
				}
				ids := []string{skills.UserSkillOwner(user)}
				for _, a := range rows {
					ids = append(ids, a.ID)
				}
				return ids, nil
			})
	}
	return ws
}
