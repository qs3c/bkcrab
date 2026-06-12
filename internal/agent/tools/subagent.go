package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/qs3c/bkclaw/internal/bus"
)

// SubAgentSpawner is the interface for spawning sub-agents.
type SubAgentSpawner interface {
	// SpawnSubAgent sends a task to another agent and returns its response.
	SpawnSubAgent(ctx context.Context, agentID string, msg bus.InboundMessage) string
}

type spawnSubagentArgs struct {
	AgentID string `json:"agentId"`
	Task    string `json:"task"`
}

// RegisterSubAgent registers the spawn_subagent tool.
func RegisterSubAgent(r *Registry, spawner SubAgentSpawner, callerAgentID string) {
	r.Register("spawn_subagent", "Spawn another agent as a sub-task and return its response. Use this to delegate work to specialized agents.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"agentId": map[string]interface{}{
				"type":        "string",
				"description": "The ID of the agent to spawn",
			},
			"task": map[string]interface{}{
				"type":        "string",
				"description": "The message/prompt to send to the sub-agent",
			},
		},
		"required": []string{"agentId", "task"},
	}, makeSubAgentTool(spawner, callerAgentID))
}

func makeSubAgentTool(spawner SubAgentSpawner, callerAgentID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args spawnSubagentArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		if args.AgentID == "" {
			return "", fmt.Errorf("agentId is required")
		}
		if args.Task == "" {
			return "", fmt.Errorf("task is required")
		}
		if args.AgentID == callerAgentID {
			return "", fmt.Errorf("cannot spawn yourself as a sub-agent")
		}

		msg := bus.InboundMessage{
			Channel:  "subagent",
			ChatID:   fmt.Sprintf("subagent-%s-%s", callerAgentID, args.AgentID),
			UserID:   callerAgentID,
			Text:     args.Task,
			PeerKind: "dm",
			Source:   bus.SourceSubAgent,
		}

		result := spawner.SpawnSubAgent(ctx, args.AgentID, msg)
		return result, nil
	}
}
