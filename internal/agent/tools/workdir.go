package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkcrab/internal/workspace"
)

func (r *Registry) scopedUserRoot() string {
	if local, ok := r.workspaceStore.(workspace.LocalScoper); ok {
		if dir, ok := local.LocalScopeDir(r.agentID, r.projectID, r.sessionID); ok {
			return dir
		}
	}
	root := r.userRoot
	if r.projectID != "" {
		root = filepath.Join(root, "projects", r.projectID)
		if r.sessionID != "" {
			root = filepath.Join(root, r.sessionID)
		}
	} else if r.sessionID != "" {
		root = filepath.Join(root, "sessions", r.sessionID)
	}
	return root
}

func (r *Registry) prepareHostWorkdir() (string, error) {
	if r == nil {
		return "", nil
	}
	for _, id := range []string{r.projectID, r.sessionID} {
		if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
			return "", fmt.Errorf("invalid workspace scope")
		}
	}
	if r.workspaceStore != nil {
		local, ok := r.workspaceStore.(workspace.LocalScoper)
		if !ok {
			return "", fmt.Errorf("host exec requires a local workspace; configure a sandbox for remote workspace storage")
		}
		if _, ok := local.LocalScopeDir(r.agentID, r.projectID, r.sessionID); !ok {
			return "", fmt.Errorf("host exec requires a local workspace; configure a sandbox for remote workspace storage")
		}
	} else if r.userRoot == "" {
		if r.sessionID != "" || r.projectID != "" {
			return "", fmt.Errorf("session workspace root is not configured")
		}
		return "", nil // Legacy tools called outside a chat.
	}
	dir, err := filepath.Abs(r.scopedUserRoot())
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
