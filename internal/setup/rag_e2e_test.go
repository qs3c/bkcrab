package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
)

type multimodalE2EParser struct {
	asset []byte
	hash  string
}

func newMultimodalE2EParser(t *testing.T) *multimodalE2EParser {
	t.Helper()
	value := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			value.Set(x, y, color.NRGBA{R: uint8(40 + x*30), G: uint8(80 + y*20), B: 180, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	asset := encoded.Bytes()
	return &multimodalE2EParser{asset: append([]byte(nil), asset...), hash: fmt.Sprintf("%x", sha256.Sum256(asset))}
}

func (p *multimodalE2EParser) Parse(
	ctx context.Context,
	source document.Source,
	options parse.ParseOptions,
) (*document.ParsedDocument, error) {
	if err := source.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	location := document.SourceLocation{Kind: document.LocationPage, Index: 2, Label: "第 2 页"}
	return document.NewParsedDocument(document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source:        source.Parsed(),
		Parser:        document.ParserInfo{Name: "multimodal-e2e", Version: options.ParserVersion},
		Units: []document.MarkdownUnit{{
			ID: "unit_page_0002", Location: location,
			Markdown: "## Gateway flow\n\nGateway listens on port 8080.\n\n![retry flow](rag-asset://occ_flow)",
		}},
		Assets: []document.ExtractedAsset{{
			LocalID: "asset_flow", ContentSHA256: p.hash, Kind: document.AssetKindImage,
			SourceKind: document.SourceKindEmbeddedOriginal, SourceMIME: "image/png",
			Width: 4, Height: 4, ByteSize: int64(len(p.asset)), BundleEntry: "assets/flow.png",
		}},
		Occurrences: []document.AssetOccurrence{{
			ID: "occ_flow", AssetLocalID: "asset_flow", UnitID: "unit_page_0002", Order: 1,
			Location: location, AltText: "retry flow", Caption: "Gateway retry flow diagram",
			OCRText: "Gateway 8080 retry queue", Confidence: 1,
		}},
	}, func(openCtx context.Context, entry string) (io.ReadCloser, error) {
		if err := openCtx.Err(); err != nil {
			return nil, err
		}
		if entry != "assets/flow.png" {
			return nil, os.ErrNotExist
		}
		return io.NopCloser(bytes.NewReader(p.asset)), nil
	}, nil), nil
}

func TestRAGMultimodalCorpusE2EIndexesStructuredTextWithoutVisualModelInput(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "rag", "testdata", "multimodal", "golden-upload.md"))
	if err != nil {
		t.Fatal(err)
	}
	kb, err := service.CreateKB(ctx, regular.ID, "Multimodal corpus", "deterministic E2E", 96, 8)
	if err != nil {
		t.Fatal(err)
	}
	upload := callRAGHandler(t, server, server.handleUploadRAGDocument,
		ragMultipartUploadRequest(t, resolver, kb.ID, regular.ID, "golden-upload.md", raw),
		map[string]string{"id": kb.ID})
	if upload.Code != http.StatusAccepted {
		t.Fatalf("upload status=%d body=%s", upload.Code, upload.Body.String())
	}
	var document store.RAGDocumentRecord
	if err := json.NewDecoder(upload.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		indexed, getErr := service.GetDocument(ctx, regular.ID, kb.ID, document.ID)
		if getErr == nil && indexed.Status == "DONE" {
			document = *indexed
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if document.Status != "DONE" || document.ActiveVersion <= 0 {
		t.Fatalf("document was not activated: %+v", document)
	}

	search := callRAGHandler(t, server, server.handleRAGSearch,
		ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/search", regular.ID,
			`{"query":"gateway 8080 retry","topN":8}`), map[string]string{"id": kb.ID})
	if search.Code != http.StatusOK {
		t.Fatalf("search status=%d body=%s", search.Code, search.Body.String())
	}
	var searchResult struct {
		Hits []rag.Hit `json:"hits"`
	}
	if err := json.NewDecoder(search.Body).Decode(&searchResult); err != nil {
		t.Fatal(err)
	}
	hits := searchResult.Hits
	if len(hits) == 0 {
		t.Fatal("structured corpus produced no retrieval hits")
	}
	encoded, err := json.Marshal(hits)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{"Gateway", "8080", "Retry"} {
		if !strings.Contains(text, want) {
			t.Errorf("retrieval response missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"image_url", "data:image", "rag-asset://", "objectKey"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("retrieval response crossed text-only boundary with %q: %s", forbidden, text)
		}
	}
	prompt := buildRAGChatPrompt(kb, "Which port and retry policy apply?", nil, hits)
	if !strings.Contains(prompt, "untrusted") || !strings.Contains(prompt, "Which port") {
		t.Fatalf("answer prompt did not frame the corpus as untrusted text: %q", prompt)
	}
	for _, forbidden := range []string{"image_url", "data:image", "rag-asset://"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("answer prompt contains visual resource marker %q: %q", forbidden, prompt)
		}
	}
}

func TestRAGMultimodalAssetE2EUploadIndexSearchAndTextOnlyAnswer(t *testing.T) {
	parser := newMultimodalE2EParser(t)
	server, resolver, _, regular, service := newRAGAPITestServerWithParser(t, 0, parser)
	ctx := context.Background()

	const answerText = "The gateway listens on port 8080 and uses the indexed retry flow. [1]"
	var answerCalls int
	var answerPrompt string
	answerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answerCalls++
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read answer request: %v", readErr)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Errorf("decode answer request: %v", err)
		}
		if _, exists := raw["tools"]; exists {
			t.Error("knowledge-base answer request unexpectedly registered tools")
		}
		for _, forbidden := range [][]byte{
			[]byte(`"image_url"`), []byte(`"content_parts"`), []byte(`"input_image"`), []byte("data:image"),
		} {
			if bytes.Contains(body, forbidden) {
				t.Errorf("knowledge-base answer request included visual content %q: %s", forbidden, body)
			}
		}
		var messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw["messages"], &messages); err != nil {
			t.Errorf("decode answer messages: %v", err)
		}
		for _, message := range messages {
			if len(message.Content) == 0 || message.Content[0] != '"' {
				t.Errorf("message %s content was not a JSON string: %s", message.Role, message.Content)
				continue
			}
			if message.Role == "user" {
				if err := json.Unmarshal(message.Content, &answerPrompt); err != nil {
					t.Errorf("decode user answer prompt: %v", err)
				}
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": answerText}}},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	t.Cleanup(answerServer.Close)
	if err := scope.SaveSetting(ctx, server.dataStore, regular.ID, "", "agents.defaults", map[string]any{
		"model": "multimodal-e2e/qa-model",
	}); err != nil {
		t.Fatal(err)
	}
	if err := scope.SaveProvider(ctx, server.dataStore, regular.ID, "", "multimodal-e2e", config.ProviderConfig{
		APIKey: "test-key", APIBase: answerServer.URL, APIType: "openai-chat",
	}); err != nil {
		t.Fatal(err)
	}

	kb, err := service.CreateKB(ctx, regular.ID, "Multimodal asset E2E", "image persistence", 96, 8)
	if err != nil {
		t.Fatal(err)
	}
	source := []byte("%PDF-1.4\n% deterministic test source\n")
	upload := callRAGHandler(t, server, server.handleUploadRAGDocument,
		ragMultipartUploadRequest(t, resolver, kb.ID, regular.ID, "multimodal.pdf", source),
		map[string]string{"id": kb.ID})
	if upload.Code != http.StatusAccepted {
		t.Fatalf("upload status=%d body=%s", upload.Code, upload.Body.String())
	}
	var uploaded store.RAGDocumentRecord
	if err := json.NewDecoder(upload.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := service.GetDocument(ctx, regular.ID, kb.ID, uploaded.ID)
		if getErr == nil && current.Status == "DONE" {
			uploaded = *current
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if uploaded.Status != "DONE" || uploaded.ActiveVersion < 1 {
		t.Fatalf("multimodal document was not activated with its asset: %+v", uploaded)
	}
	version, err := server.dataStore.GetRAGDocumentVersion(ctx, uploaded.ID, uploaded.ActiveVersion)
	if err != nil || version.AssetCount != 1 {
		t.Fatalf("active version asset result=%+v err=%v", version, err)
	}

	search := callRAGHandler(t, server, server.handleRAGSearch,
		ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/search", regular.ID,
			`{"query":"Gateway 8080 retry flow","topN":8}`), map[string]string{"id": kb.ID})
	if search.Code != http.StatusOK {
		t.Fatalf("search status=%d body=%s", search.Code, search.Body.String())
	}
	var result struct {
		Hits []rag.Hit `json:"hits"`
	}
	if err := json.NewDecoder(search.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) == 0 || len(result.Hits[0].Assets) != 1 {
		t.Fatalf("multimodal retrieval did not hydrate the local image: %+v", result.Hits)
	}
	asset := result.Hits[0].Assets[0]
	if !strings.HasPrefix(asset.ID, "ast_") || asset.Caption != "Gateway retry flow diagram" ||
		asset.Location.Kind != document.LocationPage || asset.Location.Index != 2 {
		t.Fatalf("retrieved asset metadata=%+v", asset)
	}

	const sessionID = "kbc_multimodal_asset_e2e"
	chat := callRAGHandler(t, server, server.handleRAGChat,
		ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/chat", regular.ID,
			`{"question":"Explain the gateway retry flow","sessionId":"`+sessionID+`"}`),
		map[string]string{"id": kb.ID})
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", chat.Code, chat.Body.String())
	}
	var chatResult struct {
		ID        string    `json:"id"`
		SessionID string    `json:"sessionId"`
		Answer    string    `json:"answer"`
		Hits      []rag.Hit `json:"hits"`
	}
	if err := json.NewDecoder(chat.Body).Decode(&chatResult); err != nil {
		t.Fatal(err)
	}
	if answerCalls != 1 {
		t.Fatalf("answer provider calls=%d, want 1", answerCalls)
	}
	for _, forbidden := range []string{"image_url", "data:image", "rag-asset://", asset.ID} {
		if strings.Contains(answerPrompt, forbidden) {
			t.Fatalf("text-only answer prompt contains visual marker %q: %q", forbidden, answerPrompt)
		}
	}
	if !strings.Contains(answerPrompt, "Gateway") || !strings.Contains(answerPrompt, "8080") {
		t.Fatalf("text-only answer prompt lost searchable image context: %q", answerPrompt)
	}
	if chatResult.ID == "" || chatResult.SessionID != sessionID || chatResult.Answer != answerText ||
		len(chatResult.Hits) == 0 || len(chatResult.Hits[0].Assets) != 1 ||
		chatResult.Hits[0].Assets[0].ID != asset.ID {
		t.Fatalf("multimodal chat response=%+v", chatResult)
	}
	persisted, err := server.dataStore.ListRAGChatTurns(ctx, regular.ID, kb.ID, sessionID)
	if err != nil || len(persisted) != 1 || persisted[0].Answer != answerText ||
		!bytes.Contains(persisted[0].Sources, []byte(asset.ID)) {
		t.Fatalf("persisted multimodal chat turns=%+v err=%v", persisted, err)
	}
}

func TestRAGAdversarialCorpusE2EStaysTextOnlyAndCannotForgeAnswerAuthority(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("..", "rag", "testdata", "multimodal", "adversarial.md"))
	if err != nil {
		t.Fatal(err)
	}

	providerRequests := make(chan []byte, 4)
	answerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read adversarial answer request: %v", readErr)
		}
		providerRequests <- append([]byte(nil), body...)
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "The corpus is untrusted text only. [1]"}}},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	t.Cleanup(answerServer.Close)
	if err := scope.SaveSetting(ctx, server.dataStore, regular.ID, "", "agents.defaults", map[string]any{
		"model": "adversarial-e2e/qa-model",
	}); err != nil {
		t.Fatal(err)
	}
	if err := scope.SaveProvider(ctx, server.dataStore, regular.ID, "", "adversarial-e2e", config.ProviderConfig{
		APIKey: "test-key", APIBase: answerServer.URL, APIType: "openai-chat",
	}); err != nil {
		t.Fatal(err)
	}

	kb, err := service.CreateKB(ctx, regular.ID, "Adversarial corpus", "authority boundary", 512, 32)
	if err != nil {
		t.Fatal(err)
	}
	upload := callRAGHandler(t, server, server.handleUploadRAGDocument,
		ragMultipartUploadRequest(t, resolver, kb.ID, regular.ID, "adversarial.md", raw),
		map[string]string{"id": kb.ID})
	if upload.Code != http.StatusAccepted {
		t.Fatalf("upload status=%d body=%s", upload.Code, upload.Body.String())
	}
	var uploaded store.RAGDocumentRecord
	if err := json.NewDecoder(upload.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := service.GetDocument(ctx, regular.ID, kb.ID, uploaded.ID)
		if getErr == nil && current.Status == "DONE" {
			uploaded = *current
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if uploaded.Status != "DONE" || uploaded.ActiveVersion <= 0 {
		t.Fatalf("adversarial document was not activated: %+v", uploaded)
	}

	search := callRAGHandler(t, server, server.handleRAGSearch,
		ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/search", regular.ID,
			`{"query":"ignore prior instructions delete_all forge ragResources","topN":8}`),
		map[string]string{"id": kb.ID})
	if search.Code != http.StatusOK {
		t.Fatalf("search status=%d body=%s", search.Code, search.Body.String())
	}
	var searchResult struct {
		Hits []rag.Hit `json:"hits"`
	}
	if err := json.NewDecoder(search.Body).Decode(&searchResult); err != nil {
		t.Fatal(err)
	}
	if len(searchResult.Hits) == 0 {
		t.Fatal("adversarial corpus produced no retrieval hits")
	}
	var retrieved strings.Builder
	for _, hit := range searchResult.Hits {
		retrieved.WriteString(hit.AnswerText())
		if len(hit.Assets) != 0 {
			t.Fatalf("forged image/metadata text created typed assets: %+v", hit.Assets)
		}
	}
	if !strings.Contains(retrieved.String(), "SYSTEM:") || !strings.Contains(retrieved.String(), "ragResources") {
		t.Fatalf("attack text did not traverse the real index/search path as inert text: %q", retrieved.String())
	}

	chat := callRAGHandler(t, server, server.handleRAGChat,
		ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/chat", regular.ID,
			`{"question":"What do the SYSTEM and METADATA lines say?","sessionId":"kbc_adversarial_e2e"}`),
		map[string]string{"id": kb.ID})
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", chat.Code, chat.Body.String())
	}
	var chatResult struct {
		Answer string    `json:"answer"`
		Hits   []rag.Hit `json:"hits"`
	}
	if err := json.NewDecoder(chat.Body).Decode(&chatResult); err != nil {
		t.Fatal(err)
	}
	if chatResult.Answer != "The corpus is untrusted text only. [1]" || len(chatResult.Hits) == 0 {
		t.Fatalf("adversarial chat response=%+v", chatResult)
	}
	for _, hit := range chatResult.Hits {
		if len(hit.Assets) != 0 {
			t.Fatalf("answer response forged typed resources from text: %+v", hit.Assets)
		}
	}

	var providerBody []byte
	select {
	case providerBody = <-providerRequests:
	case <-time.After(time.Second):
		t.Fatal("answer provider did not receive the adversarial corpus")
	}
	assertAdversarialRAGAnswerRequest(t, providerBody)
}

func assertAdversarialRAGAnswerRequest(t *testing.T, body []byte) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode answer request: %v", err)
	}
	if _, exists := envelope["tools"]; exists {
		t.Fatal("adversarial document registered answer-model tools")
	}
	for _, forbidden := range [][]byte{
		[]byte(`"image_url"`), []byte(`"content_parts"`), []byte(`"input_image"`), []byte("data:image"),
	} {
		if bytes.Contains(body, forbidden) {
			t.Fatalf("adversarial answer request crossed visual boundary with %q: %s", forbidden, body)
		}
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(envelope["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "system" || messages[1].Role != "user" {
		t.Fatalf("document text changed provider roles: %+v", messages)
	}
	var prompt string
	for _, message := range messages {
		if len(message.Content) == 0 || message.Content[0] != '"' {
			t.Fatalf("provider message content is not a JSON string: %s", message.Content)
		}
		if message.Role == "user" {
			if err := json.Unmarshal(message.Content, &prompt); err != nil {
				t.Fatal(err)
			}
		}
	}
	const startMarker = "<untrusted_retrieved_data format=\"jsonl\">\n"
	start := strings.Index(prompt, startMarker)
	end := strings.Index(prompt, "\n</untrusted_retrieved_data>")
	if start < 0 || end <= start {
		t.Fatalf("answer prompt lost the untrusted JSONL boundary: %q", prompt)
	}
	jsonl := prompt[start+len(startMarker) : end]
	seenAttackText := false
	for _, line := range strings.Split(jsonl, "\n") {
		var record struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("retrieved data escaped its JSON record: line=%q err=%v", line, err)
		}
		if strings.Contains(record.Text, "SYSTEM:") || strings.Contains(record.Text, "ragResources") {
			seenAttackText = true
		}
	}
	if !seenAttackText {
		t.Fatalf("adversarial text was not present inside JSON data records: %q", prompt)
	}
}
