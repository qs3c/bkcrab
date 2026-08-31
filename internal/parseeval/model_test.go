package parseeval

import (
	"strings"
	"testing"
)

func TestClosedEnums(t *testing.T) {
	for _, status := range []RunStatus{RunDraft, RunQueued, RunRunning, RunSucceeded, RunPartial, RunFailed, RunCancelled} {
		if !status.Valid() {
			t.Errorf("valid run status rejected: %q", status)
		}
	}
	for _, status := range []DocumentStatus{DocumentUploaded, DocumentRunning, DocumentSucceeded, DocumentPartial, DocumentFailed} {
		if !status.Valid() {
			t.Errorf("valid document status rejected: %q", status)
		}
	}
	for _, engine := range []Engine{EngineMarkItDown, EngineAnyDoc} {
		if !engine.Valid() {
			t.Errorf("valid engine rejected: %q", engine)
		}
	}
	for _, kind := range []ArtifactKind{ArtifactSource, ArtifactTruthPage, ArtifactMarkdown, ArtifactJudgeRaw} {
		if !kind.Valid() {
			t.Errorf("valid artifact kind rejected: %q", kind)
		}
	}
	if RunStatus("paused").Valid() || DocumentStatus("queued").Valid() || Engine("other").Valid() || ArtifactKind("object-key").Valid() {
		t.Fatal("unknown enum value accepted")
	}
}

func TestExecutionSnapshotClosedJSON(t *testing.T) {
	raw := []byte(`{
      "markitdown":{"name":"markitdown","version":"0.1.6","wrapperVersion":"office-wrapper-v3"},
      "anydoc":{"name":"anydoc","version":"0.1.9","wrapperVersion":"office-anydoc-wrapper-v2"},
      "renderer":{"protocolVersion":"parser-eval-renderer/v1","serviceVersion":"1","libreOfficeVersion":"7.4","pyMuPDFVersion":"1.26"},
      "judge":{"id":"openai/gpt-vision","provider":"openai","model":"gpt-vision","fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","modelDisplayName":"Vision","pricingKnown":true,"inputCostPerMillion":1,"outputCostPerMillion":2},
      "renderDPI":100,"maxPages":6,"markdownJudgeChars":40000,"judgePromptVersion":"parser-eval-judge-v1","appVersion":"test","createdBy":"admin","parserConcurrency":1
    }`)
	snapshot, err := DecodeClosedJSON(raw, MaxSnapshotJSONBytes, func(value *ExecutionSnapshot) error { return value.Validate() })
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Judge.Model != "gpt-vision" || snapshot.ParserConcurrency != 1 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}

	unknown := append([]byte(nil), raw...)
	unknown = []byte(strings.Replace(string(unknown), `"parserConcurrency":1`, `"parserConcurrency":1,"apiKey":"secret"`, 1))
	if _, err := DecodeClosedJSON(unknown, MaxSnapshotJSONBytes, func(value *ExecutionSnapshot) error { return value.Validate() }); err == nil {
		t.Fatal("unknown credential field accepted")
	}
	if _, err := DecodeClosedJSON(append(raw, []byte(` {}`)...), MaxSnapshotJSONBytes, func(value *ExecutionSnapshot) error { return value.Validate() }); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestExecutionSnapshotValidationAndBounds(t *testing.T) {
	snapshot := validSnapshot()
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	snapshot.Judge.Fingerprint = "short"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("short fingerprint accepted")
	}

	snapshot = validSnapshot()
	snapshot.AppVersion = strings.Repeat("x", 257)
	if err := snapshot.Validate(); err == nil {
		t.Fatal("oversized app version accepted")
	}

	snapshot = validSnapshot()
	if _, err := EncodeBoundedJSON(snapshot, 8); err == nil {
		t.Fatal("oversized encoded JSON accepted")
	}
}

func TestResultSlotSuccess(t *testing.T) {
	if !(TruthResult{Status: StepSucceeded}).Successful() || !(ParserResult{Status: StepSucceeded}).Successful() || !(JudgeSlot{Status: StepSucceeded}).Successful() {
		t.Fatal("successful slot was not recognized")
	}
	if (ParserResult{Status: StepFailed}).Successful() || (JudgeSlot{Status: StepPending}).Successful() {
		t.Fatal("incomplete slot was recognized as successful")
	}
}

func validSnapshot() ExecutionSnapshot {
	return ExecutionSnapshot{
		MarkItDown: ParserDescriptor{Name: "markitdown", Version: "0.1.6", WrapperVersion: "office-wrapper-v3"},
		AnyDoc:     ParserDescriptor{Name: "anydoc", Version: "0.1.9", WrapperVersion: "office-anydoc-wrapper-v2"},
		Renderer: RendererDescriptor{
			ProtocolVersion: "parser-eval-renderer/v1", ServiceVersion: "1",
			LibreOfficeVersion: "7.4", PyMuPDFVersion: "1.26",
		},
		Judge: JudgeBindingSnapshot{
			ID: "openai/gpt-vision", Provider: "openai", Model: "gpt-vision",
			Fingerprint: strings.Repeat("a", 64), ModelDisplayName: "Vision",
			PricingKnown: true, InputCostPerMillion: 1, OutputCostPerMillion: 2,
		},
		RenderDPI: 100, MaxPages: 6, MarkdownJudgeChars: 40_000,
		JudgePromptVersion: "parser-eval-judge-v1", AppVersion: "test", CreatedBy: "admin", ParserConcurrency: 1,
	}
}
