package imagegen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Action string

const (
	RequestSchemaVersion = 1

	ActionCreate Action = "create"
	ActionStatus Action = "status"
	ActionCancel Action = "cancel"

	SizeSquare    = "square"
	SizeLandscape = "landscape"
	SizePortrait  = "portrait"
)

var canonicalBatchID = regexp.MustCompile(`^imgb_[a-z0-9]{16,64}$`)

type ExecutionIdentity struct {
	UserID             string
	ConfigUserID       string
	AgentOwnerUserID   string
	AgentID            string
	WorkspaceProjectID string
	WorkspaceSessionID string
	MessageChannel     string
}

func (i ExecutionIdentity) Validate() error {
	for name, value := range map[string]string{
		"user ID":             i.UserID,
		"config user ID":      i.ConfigUserID,
		"agent owner user ID": i.AgentOwnerUserID,
		"agent ID":            i.AgentID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("imagegen: %s is required", name)
		}
	}
	return nil
}

type RequestLimits struct {
	MaxImagesPerBatch  int
	MaxItems           int
	PromptMaxRunes     int
	RequestMaxBytes    int64
	WaitDefaultSeconds int
	WaitMaxSeconds     int
}

type NormalizedItem struct {
	Index  int    `json:"index"`
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
	Size   string `json:"size"`
	Count  int    `json:"count"`
}

type NormalizedRequest struct {
	Version     int              `json:"version,omitempty"`
	Action      Action           `json:"action"`
	BatchID     string           `json:"batch_id,omitempty"`
	Items       []NormalizedItem `json:"items,omitempty"`
	WaitSeconds int              `json:"wait_seconds,omitempty"`
}

type rawRequest struct {
	Action      string     `json:"action"`
	BatchID     *string    `json:"batch_id"`
	Prompt      *string    `json:"prompt"`
	Count       *int       `json:"count"`
	Size        *string    `json:"size"`
	Items       *[]rawItem `json:"items"`
	WaitSeconds *int       `json:"wait_seconds"`
}

type rawItem struct {
	Label  string  `json:"label"`
	Prompt *string `json:"prompt"`
	Count  *int    `json:"count"`
	Size   *string `json:"size"`
}

func NormalizeRequest(raw json.RawMessage, limits RequestLimits) (NormalizedRequest, error) {
	if limits.MaxImagesPerBatch < 1 || limits.MaxItems < 1 || limits.PromptMaxRunes < 1 || limits.RequestMaxBytes < 1 || limits.WaitMaxSeconds < 0 {
		return NormalizedRequest{}, errors.New("imagegen: invalid request limits")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var input rawRequest
	if err := dec.Decode(&input); err != nil {
		return NormalizedRequest{}, fmt.Errorf("imagegen: parse request: %w", err)
	}
	if dec.Decode(&struct{}{}) == nil {
		return NormalizedRequest{}, errors.New("imagegen: request must contain one JSON object")
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return NormalizedRequest{}, fmt.Errorf("imagegen: parse request fields: %w", err)
	}
	action := Action(strings.ToLower(strings.TrimSpace(input.Action)))
	if action == "" {
		action = ActionCreate
	}
	switch action {
	case ActionCreate:
		return normalizeCreate(input, present, limits)
	case ActionStatus, ActionCancel:
		return normalizeBatchAction(action, input, present)
	default:
		return NormalizedRequest{}, fmt.Errorf("imagegen: unsupported action %q", input.Action)
	}
}

func normalizeBatchAction(action Action, input rawRequest, present map[string]json.RawMessage) (NormalizedRequest, error) {
	for key := range present {
		if key != "action" && key != "batch_id" {
			return NormalizedRequest{}, fmt.Errorf("imagegen: action %s only accepts batch_id", action)
		}
	}
	if input.BatchID == nil || !canonicalBatchID.MatchString(strings.TrimSpace(*input.BatchID)) {
		return NormalizedRequest{}, errors.New("imagegen: canonical batch_id is required")
	}
	return NormalizedRequest{Action: action, BatchID: strings.TrimSpace(*input.BatchID)}, nil
}

func normalizeCreate(input rawRequest, present map[string]json.RawMessage, limits RequestLimits) (NormalizedRequest, error) {
	if _, ok := present["batch_id"]; ok {
		return NormalizedRequest{}, errors.New("imagegen: create does not accept batch_id")
	}
	hasPrompt := input.Prompt != nil
	hasItems := input.Items != nil
	if hasPrompt == hasItems {
		return NormalizedRequest{}, errors.New("imagegen: create requires exactly one of prompt or items")
	}
	wait := limits.WaitDefaultSeconds
	if input.WaitSeconds != nil {
		wait = *input.WaitSeconds
	}
	if wait < 0 || wait > limits.WaitMaxSeconds {
		return NormalizedRequest{}, fmt.Errorf("imagegen: wait_seconds must be in [0,%d]", limits.WaitMaxSeconds)
	}
	var source []rawItem
	if hasPrompt {
		source = []rawItem{{Prompt: input.Prompt, Count: input.Count, Size: input.Size}}
	} else {
		if _, countPresent := present["count"]; countPresent || present["size"] != nil || present["prompt"] != nil {
			return NormalizedRequest{}, errors.New("imagegen: top-level prompt, count, and size are only valid in prompt mode")
		}
		source = *input.Items
		if len(source) < 1 || len(source) > limits.MaxItems {
			return NormalizedRequest{}, fmt.Errorf("imagegen: items must contain between 1 and %d entries", limits.MaxItems)
		}
	}
	items := make([]NormalizedItem, 0, len(source))
	labels := make(map[string]struct{}, len(source))
	total := 0
	for index, item := range source {
		if item.Prompt == nil || strings.TrimSpace(*item.Prompt) == "" {
			return NormalizedRequest{}, fmt.Errorf("imagegen: item %d prompt is required", index)
		}
		if !utf8.ValidString(*item.Prompt) || utf8.RuneCountInString(*item.Prompt) > limits.PromptMaxRunes {
			return NormalizedRequest{}, fmt.Errorf("imagegen: item %d prompt exceeds %d Unicode code points", index, limits.PromptMaxRunes)
		}
		count := 1
		if item.Count != nil {
			count = *item.Count
		}
		if count < 1 {
			return NormalizedRequest{}, fmt.Errorf("imagegen: item %d count must be positive", index)
		}
		total += count
		if total > limits.MaxImagesPerBatch {
			return NormalizedRequest{}, fmt.Errorf("imagegen: total image count exceeds %d", limits.MaxImagesPerBatch)
		}
		size := SizeSquare
		if item.Size != nil && strings.TrimSpace(*item.Size) != "" {
			size = strings.ToLower(strings.TrimSpace(*item.Size))
		}
		if size != SizeSquare && size != SizeLandscape && size != SizePortrait {
			return NormalizedRequest{}, fmt.Errorf("imagegen: item %d has unsupported size %q", index, size)
		}
		label := strings.TrimSpace(item.Label)
		if label == "" {
			label = fmt.Sprintf("item-%d", index)
		}
		if utf8.RuneCountInString(label) > 191 {
			return NormalizedRequest{}, fmt.Errorf("imagegen: item %d label is too long", index)
		}
		if _, exists := labels[label]; exists {
			return NormalizedRequest{}, fmt.Errorf("imagegen: duplicate normalized label %q", label)
		}
		labels[label] = struct{}{}
		items = append(items, NormalizedItem{Index: index, Label: label, Prompt: *item.Prompt, Size: size, Count: count})
	}
	if total < 1 {
		return NormalizedRequest{}, errors.New("imagegen: total image count must be positive")
	}
	result := NormalizedRequest{Version: RequestSchemaVersion, Action: ActionCreate, Items: items, WaitSeconds: wait}
	normalizedJSON, err := json.Marshal(result)
	if err != nil {
		return NormalizedRequest{}, fmt.Errorf("imagegen: encode normalized request: %w", err)
	}
	if int64(len(normalizedJSON)) > limits.RequestMaxBytes {
		return NormalizedRequest{}, fmt.Errorf("imagegen: normalized request exceeds %d bytes", limits.RequestMaxBytes)
	}
	return result, nil
}
