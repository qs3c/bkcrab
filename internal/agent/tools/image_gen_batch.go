package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/imagegen"
)

var ErrImagegenBatchDraining = errors.New("imagegen: batch creation is draining")

type ImagegenBatchService interface {
	Create(context.Context, imagegen.ExecutionIdentity, imagegen.NormalizedRequest) (imagegen.BatchResult, error)
	Status(context.Context, imagegen.ExecutionIdentity, string) (imagegen.BatchResult, error)
	Cancel(context.Context, imagegen.ExecutionIdentity, string) (imagegen.BatchResult, error)
}

// RegisterImageGenBatch exposes the durable batch protocol in fair and drain
// modes. Registration deliberately does not depend on transient Rabbit, Redis,
// object-store, or provider readiness: status/cancel remain useful and create
// commits to MySQL before best-effort dispatch.
func RegisterImageGenBatch(r *Registry, cfg config.ImagegenBatchCfg, service ImagegenBatchService) {
	if r == nil || cfg.Mode == config.ImagegenBatchModeLegacy || service == nil {
		return
	}
	limits := imagegen.RequestLimits{
		MaxImagesPerBatch:  cfg.MaxImagesPerBatch,
		MaxItems:           16,
		PromptMaxRunes:     cfg.PromptMaxRunes,
		RequestMaxBytes:    cfg.RequestMaxBytes,
		WaitDefaultSeconds: int(cfg.ToolWaitDefault.Seconds()),
		WaitMaxSeconds:     int(cfg.ToolWaitMax.Seconds()),
	}
	parameters := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []string{"create", "status", "cancel"}, "default": "create"},
			"batch_id": map[string]any{"type": "string", "description": "Canonical batch ID for status or cancel"},
			"prompt":   map[string]any{"type": "string"},
			"count":    map[string]any{"type": "integer", "minimum": 1, "maximum": cfg.MaxImagesPerBatch},
			"size":     map[string]any{"type": "string", "enum": []string{imagegen.SizeSquare, imagegen.SizeLandscape, imagegen.SizePortrait}},
			"items": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 16,
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{
						"label": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"},
						"count": map[string]any{"type": "integer", "minimum": 1, "maximum": cfg.MaxImagesPerBatch},
						"size":  map[string]any{"type": "string", "enum": []string{imagegen.SizeSquare, imagegen.SizeLandscape, imagegen.SizePortrait}},
					}, "required": []string{"prompt"},
				},
			},
			"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": limits.WaitMaxSeconds},
		},
	}
	description := "Create a durable image batch (up to 16 images), or inspect/cancel one with action=status/cancel. Submission may return a batch ID before completion; use it later and 不要在同一轮高频轮询。"
	r.RegisterResult("image_gen_batch", description, parameters, func(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
		request, err := imagegen.NormalizeRequest(raw, limits)
		if err != nil {
			return ToolResult{}, fmt.Errorf("invalid image_gen_batch arguments: %w", err)
		}
		identity := r.ImagegenExecutionIdentity()
		var result imagegen.BatchResult
		switch request.Action {
		case imagegen.ActionCreate:
			if cfg.Mode == config.ImagegenBatchModeDrain {
				return ToolResult{}, ErrImagegenBatchDraining
			}
			result, err = service.Create(ctx, identity, request)
		case imagegen.ActionStatus:
			result, err = service.Status(ctx, identity, request.BatchID)
		case imagegen.ActionCancel:
			result, err = service.Cancel(ctx, identity, request.BatchID)
		default:
			err = fmt.Errorf("imagegen: unsupported action %q", request.Action)
		}
		if err != nil {
			return ToolResult{}, err
		}
		// Workspace paths and origin are the delivery authority. Provider URLs
		// can contain signed query credentials, so they never cross this boundary.
		public := result
		public.Artifacts = append([]imagegen.BatchArtifactResult(nil), result.Artifacts...)
		for i := range public.Artifacts {
			public.Artifacts[i].URL = ""
		}
		text, err := json.Marshal(public)
		if err != nil {
			return ToolResult{}, fmt.Errorf("encode image_gen_batch result: %w", err)
		}
		toolResult := ToolResult{Text: string(text)}
		if len(public.Artifacts) > 0 {
			rawArtifacts, marshalErr := json.Marshal(public.Artifacts)
			if marshalErr != nil {
				return ToolResult{}, fmt.Errorf("encode image artifact metadata: %w", marshalErr)
			}
			toolResult.Metadata = ResultMetadata{ImageArtifactsMetadataKey: rawArtifacts}
		}
		return toolResult, nil
	})
}
