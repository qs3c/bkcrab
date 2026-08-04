package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/agent/tools"
)

func loopImageArtifact(index, item int, session string) tools.ImageArtifactRef {
	hash := strings.Repeat(string(rune('a'+index)), 64)
	ref := tools.ImageArtifactRef{
		BatchID: "imgb_0000000000000001", TaskID: "imgt_0000000000000001",
		ItemIndex: item, ChunkIndex: 0, Index: index, MIMEType: "image/png",
		Size: 12, Width: 2, Height: 2, SHA256: hash,
		Path: "imagegen/imgb_0000000000000001/imgt_0000000000000001/claims/1/image-" + string(rune('0'+index)) + "-" + hash + ".png",
	}
	ref.Origin.AgentID = "agent_1"
	ref.Origin.ProjectID = "project-old"
	ref.Origin.SessionID = session
	return ref
}

func imageArtifactMetadata(t *testing.T, refs ...tools.ImageArtifactRef) tools.ResultMetadata {
	t.Helper()
	raw, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	return tools.ResultMetadata{tools.ImageArtifactsMetadataKey: raw}
}

func TestImagegenArtifactMetadataAccumulatesDeduplicatesAndKeepsOrigin(t *testing.T) {
	artifacts := newTurnImageArtifacts()
	second := loopImageArtifact(1, 1, "session-before")
	first := loopImageArtifact(0, 0, "session-before")
	artifacts.add("image_gen_batch", nil, imageArtifactMetadata(t, second))
	artifacts.add("image_gen_batch", nil, imageArtifactMetadata(t, first, second))
	artifacts.add("image_gen_batch", errors.New("status failed"), imageArtifactMetadata(t, loopImageArtifact(2, 2, "bad")))
	artifacts.add("other_tool", nil, imageArtifactMetadata(t, loopImageArtifact(2, 2, "forged")))

	merged := artifacts.merge(map[string]any{"loopGuardReached": true})
	refs, ok := merged[tools.ImageArtifactsMetadataKey].([]tools.ImageArtifactRef)
	if !ok || len(refs) != 2 {
		t.Fatalf("merged refs = %#v", merged[tools.ImageArtifactsMetadataKey])
	}
	if refs[0].ItemIndex != 0 || refs[1].ItemIndex != 1 {
		t.Fatalf("refs are not stable: %+v", refs)
	}
	if refs[0].Origin.SessionID != "session-before" || refs[0].Origin.ProjectID != "project-old" {
		t.Fatalf("origin was rebound to current session: %+v", refs[0].Origin)
	}
	if merged["loopGuardReached"] != true {
		t.Fatal("existing final metadata was lost")
	}
}
