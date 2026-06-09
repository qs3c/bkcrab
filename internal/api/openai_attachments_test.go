package api

import (
	"encoding/json"
	"testing"
)

// Wire round-trip for the OpenAI-compat endpoint: a client posts a body
// using all three attachment shapes and the helpers split them into the
// right buckets.
func TestChatCompletionRequestAttachmentsRoundTrip(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-7",
		"messages": [{"role": "user", "content": "hi"}],
		"images":    ["data:image/png;base64,AAA"],
		"imageUrls": ["https://x/photo.jpg"],
		"attachments": [
			{"url": "data:application/pdf;base64,BBB", "name": "Q4-report.pdf"},
			{"url": "https://x/archive.zip"}
		]
	}`)
	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(req.Attachments) != 2 || req.Attachments[0].Name != "Q4-report.pdf" {
		t.Errorf("Attachments = %+v", req.Attachments)
	}

	inline := req.inlineImageURLs()
	if len(inline) != 2 {
		t.Fatalf("inline len = %d, want 2 (Images + ImageURLs only)", len(inline))
	}
	if inline[0] != "data:image/png;base64,AAA" || inline[1] != "https://x/photo.jpg" {
		t.Errorf("inline order/values wrong: %v", inline)
	}
	// Critical: a PDF / zip data URL must not leak into the inline-vision
	// slice — that would be wrapped as an image_url content block and
	// upstream providers reject the whole turn.
	for _, u := range inline {
		if u == "data:application/pdf;base64,BBB" || u == "https://x/archive.zip" {
			t.Errorf("non-image URL leaked into inline: %q", u)
		}
	}

	all := req.allAttachments()
	if len(all) != 4 {
		t.Errorf("allAttachments len = %d, want 4 (Images+ImageURLs+Attachments)", len(all))
	}
}

// imageUrls alone (without the Images alias) must still flow through —
// this was the original bug C: the OpenAI endpoint silently dropped it.
func TestChatCompletionImageUrlsAliasAlone(t *testing.T) {
	body := []byte(`{
		"model": "x",
		"messages": [],
		"imageUrls": ["https://x/a.png"]
	}`)
	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := req.inlineImageURLs(); len(got) != 1 || got[0] != "https://x/a.png" {
		t.Errorf("inlineImageURLs = %v", got)
	}
	if got := req.allAttachments(); len(got) != 1 || got[0].URL != "https://x/a.png" {
		t.Errorf("allAttachments = %+v", got)
	}
}
