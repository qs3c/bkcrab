package session

import (
	"encoding/json"
	"github.com/qs3c/bkcrab/internal/provider"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWebSessionPreviewPreservesUnicode(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "database"}[durable], func(t *testing.T) {
			root := t.TempDir()
			m := NewManager(root)
			if durable {
				m = NewManagerWithStoreForUser(t.TempDir(), NewStoreAdapter(sessionRegressionDB(t), "u"), "u", "a")
			}
			text := "你好，这是作品体验验收。" + strings.Repeat("中文🙂", 55)
			s := m.Get("web", "", "unicode", "")
			s.Append(provider.Message{Role: "user", Content: text})
			if !durable {
				// The file scanner supports historical web_<id>.jsonl files.
				payload, _ := json.Marshal(provider.Message{Role: "user", Content: text})
				if err := os.WriteFile(filepath.Join(root, "web_unicode.jsonl"), append(payload, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.PersistenceError(); err != nil {
				t.Fatal(err)
			}
			rows := m.ListWebSessions()
			if len(rows) != 1 {
				t.Fatalf("sessions=%d", len(rows))
			}
			for _, value := range []string{rows[0].Title, rows[0].Preview} {
				if !utf8.ValidString(value) || strings.Contains(value, "�") {
					t.Fatalf("broken Unicode: %q", value)
				}
				if len([]rune(value)) > 103 {
					t.Fatalf("preview not bounded: %d", len([]rune(value)))
				}
			}
		})
	}
}
