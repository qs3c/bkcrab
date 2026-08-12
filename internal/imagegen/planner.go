package imagegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

type PlannedTask struct {
	ItemIndex          int
	ChunkIndex         int
	Label              string
	Prompt             string
	Size               string
	RequestedCount     int
	RequestFingerprint string
}

func PlanTasks(request NormalizedRequest, maxImagesPerTask int) ([]PlannedTask, error) {
	if request.Version != RequestSchemaVersion || request.Action != ActionCreate || len(request.Items) == 0 {
		return nil, errors.New("imagegen: planner requires a normalized create request")
	}
	if maxImagesPerTask < 1 || maxImagesPerTask > 4 {
		return nil, fmt.Errorf("imagegen: max images per task must be in [1,4], got %d", maxImagesPerTask)
	}
	tasks := make([]PlannedTask, 0, len(request.Items))
	for position, item := range request.Items {
		if item.Index != position || item.Count < 1 {
			return nil, fmt.Errorf("imagegen: item %d is not canonically indexed or counted", position)
		}
		remaining := item.Count
		for chunk := 0; remaining > 0; chunk++ {
			count := maxImagesPerTask
			if remaining < count {
				count = remaining
			}
			fingerprint, err := taskFingerprint(item, chunk, count)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, PlannedTask{
				ItemIndex: item.Index, ChunkIndex: chunk, Label: item.Label,
				Prompt: item.Prompt, Size: item.Size, RequestedCount: count,
				RequestFingerprint: fingerprint,
			})
			remaining -= count
		}
	}
	return tasks, nil
}

func taskFingerprint(item NormalizedItem, chunk, count int) (string, error) {
	payload := struct {
		Version   int    `json:"version"`
		ItemIndex int    `json:"item_index"`
		Chunk     int    `json:"chunk_index"`
		Label     string `json:"label"`
		Prompt    string `json:"prompt"`
		Size      string `json:"size"`
		Count     int    `json:"count"`
	}{Version: 1, ItemIndex: item.Index, Chunk: chunk, Label: item.Label, Prompt: item.Prompt, Size: item.Size, Count: count}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("imagegen: fingerprint request: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
