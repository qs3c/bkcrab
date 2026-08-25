// Package dialogue defines the role-aware conversation history shared by
// production RAG and evaluation adapters.
package dialogue

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

func NormalizeRole(value string) Role {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "user":
		return RoleUser
	case "assistant", "agent":
		return RoleAssistant
	default:
		return ""
	}
}

type Turn struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

func NewTurn(role, content string) Turn {
	return Turn{Role: NormalizeRole(role), Content: strings.TrimSpace(content)}
}

func (t Turn) Valid() bool {
	return (t.Role == RoleUser || t.Role == RoleAssistant) && strings.TrimSpace(t.Content) != ""
}

// UnmarshalJSON preserves compatibility with evaluation datasets created
// before role-aware history existed. A legacy string represented a prior user
// question, while new datasets serialize an explicit {role,content} object.
func (t *Turn) UnmarshalJSON(data []byte) error {
	if t == nil {
		return errors.New("nil dialogue turn receiver")
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty dialogue turn")
	}
	if data[0] == '"' {
		var content string
		if err := json.Unmarshal(data, &content); err != nil {
			return err
		}
		*t = NewTurn(string(RoleUser), content)
		return nil
	}
	var raw struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*t = NewTurn(raw.Role, raw.Content)
	return nil
}

func Clone(history []Turn) []Turn {
	return append([]Turn(nil), history...)
}

// Normalize keeps the most recent valid turns within both limits. The newest
// turn is retained first when the rune budget cannot hold the whole history.
func Normalize(history []Turn, maxTurns, maxRunes int) []Turn {
	if maxTurns <= 0 || maxRunes <= 0 || len(history) == 0 {
		return []Turn{}
	}
	if len(history) > maxTurns {
		history = history[len(history)-maxTurns:]
	}
	reversed := make([]Turn, 0, len(history))
	remaining := maxRunes
	for index := len(history) - 1; index >= 0 && remaining > 0; index-- {
		turn := NewTurn(string(history[index].Role), history[index].Content)
		if !turn.Valid() {
			continue
		}
		runes := []rune(turn.Content)
		if len(runes) > remaining {
			if len(reversed) > 0 {
				break
			}
			turn.Content = strings.TrimSpace(string(runes[:remaining]))
		}
		if !turn.Valid() {
			continue
		}
		reversed = append(reversed, turn)
		remaining -= utf8.RuneCountInString(turn.Content)
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}
