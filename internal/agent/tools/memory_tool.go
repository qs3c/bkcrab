package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qs3c/bkclaw/internal/memory"
)

type memoryToolArgs struct {
	Target     memory.Target       `json:"target"`
	Action     memory.Action       `json:"action"`
	Content    string              `json:"content,omitempty"`
	OldText    string              `json:"old_text,omitempty"`
	Operations *[]memory.Operation `json:"operations,omitempty"`
}

func registerMemory(r *Registry) {
	r.Register("memory", "Manage the current chatter's USER.md profile and MEMORY.md long-term memory with safe add/list/replace/remove operations", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"enum":        []string{"user", "memory"},
				"description": "Which managed file to operate on: user maps to USER.md, memory maps to MEMORY.md.",
			},
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"list", "add", "replace", "remove"},
				"description": "Single operation action. Missing or empty means list.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Entry content for add or replacement content for replace.",
			},
			"old_text": map[string]any{
				"type":        "string",
				"description": "Unique text identifying the entry to replace or remove.",
			},
			"operations": map[string]any{
				"type":        "array",
				"description": "Optional batch of operations applied atomically to one target.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": map[string]any{
							"type": "string",
							"enum": []string{"list", "add", "replace", "remove"},
						},
						"content": map[string]any{
							"type": "string",
						},
						"old_text": map[string]any{
							"type": "string",
						},
					},
				},
			},
		},
		"required": []string{"target"},
	}, makeMemoryTool(r))
}

func makeMemoryTool(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args memoryToolArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		target, filename, err := parseMemoryTarget(args.Target)
		if err != nil {
			return "", err
		}
		manager := memory.NewManager(memory.Options{
			Store:   r.systemFileStore,
			Root:    r.systemRoot,
			AgentID: r.agentID,
			UserID:  r.systemFileUserID(filename),
			Config:  r.managedMemoryCfg,
		})

		ops, mutation := memoryToolOperations(args)
		var result memory.Result
		if len(ops) == 1 && ops[0].Action == memory.ActionList {
			result = manager.List(ctx, target)
		} else {
			result = manager.Apply(ctx, target, ops)
		}
		if result.Success && mutation {
			result.Entries = nil
		}
		out, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(out), nil
	}
}

func parseMemoryTarget(target memory.Target) (memory.Target, string, error) {
	target = memory.Target(strings.TrimSpace(string(target)))
	switch target {
	case memory.TargetUser, memory.TargetMemory:
		filename, err := memory.Filename(target)
		return target, filename, err
	default:
		return "", "", fmt.Errorf("memory target must be %q or %q", memory.TargetUser, memory.TargetMemory)
	}
}

func memoryToolOperations(args memoryToolArgs) ([]memory.Operation, bool) {
	if args.Operations != nil {
		ops := append([]memory.Operation(nil), (*args.Operations)...)
		return ops, memoryToolHasMutation(ops)
	}
	action := args.Action
	if strings.TrimSpace(string(action)) == "" {
		action = memory.ActionList
	}
	ops := []memory.Operation{{
		Action:  action,
		Content: args.Content,
		OldText: args.OldText,
	}}
	return ops, action != memory.ActionList
}

func memoryToolHasMutation(ops []memory.Operation) bool {
	for _, op := range ops {
		if op.Action != memory.ActionList {
			return true
		}
	}
	return false
}
