package agent

import (
	"reflect"
	"testing"

	"github.com/bkclaw-ai/bkclaw/internal/bus"
	"github.com/bkclaw-ai/bkclaw/internal/provider"
)

// Origin tagging guards the compaction / WebChatHistory / FTS
// filters that check Origin != OriginUser. Before this was wired
// the field stayed "" on goal continuations and all three filters
// silently no-op'd. Cover every declared Source so a future
// rename or added source is caught at build/test time.
func TestBuildUserMessageOriginPropagates(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{"user turn", bus.SourceUser, provider.OriginUser},
		{"cron tick", bus.SourceCron, provider.OriginUser},
		{"heartbeat", bus.SourceHeartbeat, provider.OriginUser},
		{"subagent", bus.SourceSubAgent, provider.OriginUser},
		{"goal context", bus.SourceGoalContext, provider.OriginGoalContext},
		{"unknown future source", "unknown", provider.OriginUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildUserMessage(bus.InboundMessage{Source: tc.source, Text: "hello"})
			if got.Origin != tc.want {
				t.Errorf("Origin = %q, want %q", got.Origin, tc.want)
			}
		})
	}
}

// Text-only message: Content stays a bare string and ContentParts
// is nil so the provider sends the cheap single-string shape.
func TestBuildUserMessagePlainText(t *testing.T) {
	got := buildUserMessage(bus.InboundMessage{Text: "hi"})
	if got.Role != "user" || got.Content != "hi" || got.ContentParts != nil {
		t.Errorf("plain-text shape wrong: %+v", got)
	}
}

// Legacy IM bridge PhotoURL → ContentParts with text + image.
func TestBuildUserMessageSingleLegacyPhoto(t *testing.T) {
	got := buildUserMessage(bus.InboundMessage{
		Text:     "look",
		PhotoURL: "https://im.example/photo.jpg",
	})
	if got.Content != "" {
		t.Errorf("Content should be blanked when ContentParts present, got %q", got.Content)
	}
	if len(got.ContentParts) != 2 {
		t.Fatalf("expected 2 parts (text + image), got %d", len(got.ContentParts))
	}
	if got.ContentParts[0].Type != "text" || got.ContentParts[0].Text != "look" {
		t.Errorf("part[0] = %+v, want {text: 'look'}", got.ContentParts[0])
	}
	if got.ContentParts[1].Type != "image_url" || got.ContentParts[1].ImageURL == nil ||
		got.ContentParts[1].ImageURL.URL != "https://im.example/photo.jpg" {
		t.Errorf("part[1] = %+v, want image_url for the photo", got.ContentParts[1])
	}
}

// Web upload PhotoURLs slice — order must be preserved.
func TestBuildUserMessageMultiplePhotoURLs(t *testing.T) {
	got := buildUserMessage(bus.InboundMessage{
		Text:      "caption",
		PhotoURLs: []string{"https://web/a.png", "https://web/b.png", "https://web/c.png"},
	})
	if len(got.ContentParts) != 4 {
		t.Fatalf("expected 4 parts (text + 3 images), got %d", len(got.ContentParts))
	}
	urls := []string{
		got.ContentParts[1].ImageURL.URL,
		got.ContentParts[2].ImageURL.URL,
		got.ContentParts[3].ImageURL.URL,
	}
	want := []string{"https://web/a.png", "https://web/b.png", "https://web/c.png"}
	if !reflect.DeepEqual(urls, want) {
		t.Errorf("image URLs out of order: got %v, want %v", urls, want)
	}
}

// Image-only sends must NOT emit a leading {text: ""} part —
// some upstreams reject content-less wire messages.
func TestBuildUserMessageImageOnly(t *testing.T) {
	got := buildUserMessage(bus.InboundMessage{
		PhotoURL: "https://im.example/photo.jpg",
	})
	if len(got.ContentParts) != 1 || got.ContentParts[0].Type != "image_url" {
		t.Errorf("image-only shape wrong: %+v", got.ContentParts)
	}
}

// PhotoURL (legacy single) is prepended before PhotoURLs (web
// slice) — bridges that set both must see the legacy one land
// first.
func TestBuildUserMessageMergesPhotoURLAndPhotoURLs(t *testing.T) {
	got := buildUserMessage(bus.InboundMessage{
		Text:      "see attachments",
		PhotoURL:  "https://legacy/first.jpg",
		PhotoURLs: []string{"https://web/second.png", "https://web/third.png"},
	})
	if len(got.ContentParts) != 4 {
		t.Fatalf("expected 4 parts, got %d", len(got.ContentParts))
	}
	if got.ContentParts[1].ImageURL.URL != "https://legacy/first.jpg" {
		t.Errorf("legacy PhotoURL should be first image, got %q", got.ContentParts[1].ImageURL.URL)
	}
}

// Goal-context turn with body: Origin AND audit prompt text both
// have to survive — the load-bearing combination.
func TestBuildUserMessageGoalContextWithText(t *testing.T) {
	got := buildUserMessage(bus.InboundMessage{
		Source: bus.SourceGoalContext,
		Text:   "<goal_context>...audit prompt...</goal_context>",
	})
	if got.Origin != provider.OriginGoalContext {
		t.Errorf("Origin = %q, want %q", got.Origin, provider.OriginGoalContext)
	}
	if got.Content != "<goal_context>...audit prompt...</goal_context>" {
		t.Errorf("audit prompt body dropped or mangled: %q", got.Content)
	}
}
