package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/qs3c/bkcrab/internal/provider"
)

var syntheticToolCallCounter uint64

func (a *Agent) normalizeToolCallIDs(resp *provider.Response) {
	if resp == nil || len(resp.ToolCalls) == 0 {
		return
	}

	seen := make(map[string]struct{}, len(resp.ToolCalls))
	changed := false
	for i := range resp.ToolCalls {
		id := strings.TrimSpace(resp.ToolCalls[i].ID)
		_, duplicate := seen[id]
		if id == "" || duplicate {
			oldID := resp.ToolCalls[i].ID
			id = newSyntheticToolCallID(i, seen)
			resp.ToolCalls[i].ID = id
			changed = true
			if oldID == "" {
				slog.Warn("synthesized missing tool_call id",
					"agent", a.name,
					"tool", resp.ToolCalls[i].Function.Name,
					"id", id)
			} else {
				slog.Warn("rewrote duplicate tool_call id",
					"agent", a.name,
					"tool", resp.ToolCalls[i].Function.Name,
					"old_id", oldID,
					"id", id)
			}
		} else if resp.ToolCalls[i].ID != id {
			resp.ToolCalls[i].ID = id
			changed = true
		}
		seen[id] = struct{}{}
	}

	if changed {
		resp.RawAssistant = rewriteRawAssistantToolCalls(resp.RawAssistant, resp.ToolCalls)
	}
}

func newSyntheticToolCallID(index int, seen map[string]struct{}) string {
	for {
		id := fmt.Sprintf("call_bkcrab_%s_%d", randomToolCallIDSuffix(), index)
		if _, exists := seen[id]; !exists {
			return id
		}
	}
}

func randomToolCallIDSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	n := atomic.AddUint64(&syntheticToolCallCounter, 1)
	return fmt.Sprintf("%x_%x", time.Now().UnixNano(), n)
}

func rewriteRawAssistantToolCalls(raw json.RawMessage, calls []provider.ToolCall) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}

	var blockType string
	if rawType, ok := obj["type"]; ok {
		_ = json.Unmarshal(rawType, &blockType)
	}
	if blockType == "thinking" {
		return raw
	}

	encoded, err := json.Marshal(calls)
	if err != nil {
		return raw
	}
	obj["tool_calls"] = encoded
	if _, ok := obj["role"]; !ok {
		obj["role"] = json.RawMessage(`"assistant"`)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}
