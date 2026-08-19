package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

func TestDecodeEvalJSONRejectsTrailingGarbage(t *testing.T) {
	for _, body := range []string{`{"name":"ok"} garbage`, `{"name":"ok"}{"name":"second"}`} {
		t.Run(body, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/", strings.NewReader(body))
			var target struct {
				Name string `json:"name"`
			}
			if decodeEvalJSON(recorder, request, 1024, &target) {
				t.Fatal("trailing content was accepted")
			}
			if recorder.Code != 400 {
				t.Fatalf("status=%d", recorder.Code)
			}
		})
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/", strings.NewReader("{\"name\":\"ok\"} \n\t"))
	var target struct {
		Name string `json:"name"`
	}
	if !decodeEvalJSON(recorder, request, 1024, &target) || target.Name != "ok" {
		t.Fatalf("valid JSON rejected: status=%d target=%+v", recorder.Code, target)
	}
}

func TestDecodeCatalogImportJSONUsesFlatPublicContract(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/", strings.NewReader(`{"datasetId":"d1","catalogId":"next-tat-tatqa","track":"TEXT_RAG","split":"dev","sampleSize":25,"seed":42}`))
	var target struct {
		DatasetID string `json:"datasetId"`
		eval.CatalogImportOptions
	}
	if !decodeEvalJSON(recorder, request, 4096, &target) {
		t.Fatalf("flat catalog request rejected: %s", recorder.Body.String())
	}
	if target.DatasetID != "d1" || target.CatalogID != eval.CatalogTATQA || target.Track != eval.DatasetTrackTextRAG || target.SampleSize != 25 {
		t.Fatalf("decoded catalog request=%+v", target)
	}
}

func TestListRAGEvalRunPageScansPastFilteredStoragePage(t *testing.T) {
	all := make([]store.RAGEvalRunRecord, 0, 203)
	for i := 0; i < 203; i++ {
		status := store.RAGEvalRunFailed
		if i >= 201 {
			status = store.RAGEvalRunSucceeded
		}
		all = append(all, store.RAGEvalRunRecord{ID: fmt.Sprintf("rer_%03d", i), Status: status})
	}
	list := func(_ context.Context, cursor string, limit int) ([]store.RAGEvalRunRecord, error) {
		out := make([]store.RAGEvalRunRecord, 0, limit)
		for _, item := range all {
			if item.ID > cursor && len(out) < limit {
				out = append(out, item)
			}
		}
		return out, nil
	}
	items, next, err := listRAGEvalRunPage(context.Background(), "", 2, store.RAGEvalRunSucceeded, list)
	if err != nil || len(items) != 2 || items[0].ID != "rer_201" || items[1].ID != "rer_202" || next != "rer_202" {
		t.Fatalf("items=%+v next=%q err=%v", items, next, err)
	}
}

func TestListRAGEvalCasePageScansPastFilteredStoragePage(t *testing.T) {
	all := make([]store.RAGEvalCaseResultRecord, 0, 202)
	for i := 0; i < 202; i++ {
		status := store.RAGEvalCaseOK
		if i == 201 {
			status = store.RAGEvalCaseError
		}
		all = append(all, store.RAGEvalCaseResultRecord{CaseID: fmt.Sprintf("case_%03d", i), Status: status})
	}
	list := func(_ context.Context, _ string, cursor string, limit int) ([]store.RAGEvalCaseResultRecord, error) {
		out := make([]store.RAGEvalCaseResultRecord, 0, limit)
		for _, item := range all {
			if item.CaseID > cursor && len(out) < limit {
				out = append(out, item)
			}
		}
		return out, nil
	}
	items, next, err := listRAGEvalCasePage(context.Background(), "run", "", 1, store.RAGEvalCaseError, list)
	if err != nil || len(items) != 1 || items[0].CaseID != "case_201" || next != "case_201" {
		t.Fatalf("items=%+v next=%q err=%v", items, next, err)
	}
}

func TestListRAGEvalMetricsForCasesReadsMoreThanTwoHundred(t *testing.T) {
	all := make([]store.RAGEvalMetricResultRecord, 0, 250)
	for i := 0; i < 250; i++ {
		all = append(all, store.RAGEvalMetricResultRecord{CaseID: "case_a", MetricName: fmt.Sprintf("metric_%03d", i), MetricVersion: "v1"})
	}
	list := func(_ context.Context, _ string, cursor string, limit int) ([]store.RAGEvalMetricResultRecord, error) {
		parts := strings.SplitN(cursor, ":", 3)
		cursorCase, cursorMetric, cursorVersion := parts[0], "", ""
		if len(parts) > 1 {
			cursorMetric = parts[1]
		}
		if len(parts) > 2 {
			cursorVersion = parts[2]
		}
		out := make([]store.RAGEvalMetricResultRecord, 0, limit)
		for _, item := range all {
			after := item.CaseID > cursorCase || (item.CaseID == cursorCase && (item.MetricName > cursorMetric || (item.MetricName == cursorMetric && item.MetricVersion > cursorVersion)))
			if after && len(out) < limit {
				out = append(out, item)
			}
		}
		return out, nil
	}
	metrics, err := listRAGEvalMetricsForCases(context.Background(), "run", []store.RAGEvalCaseResultRecord{{CaseID: "case_a"}}, list)
	if err != nil || len(metrics) != 250 {
		t.Fatalf("metric count=%d err=%v", len(metrics), err)
	}
}

func TestEvalCaseDTOEncodesEmptyMetricsAndErrorMessage(t *testing.T) {
	encoded, err := json.Marshal(evalCaseDTO{CaseID: "case-1", Status: store.RAGEvalCaseError, ErrorCode: "search_error", ErrorMessage: "reranker timeout", Metrics: []evalMetricDTO{}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	metrics, ok := decoded["metrics"].([]any)
	if !ok || len(metrics) != 0 {
		t.Fatalf("metrics must be an empty JSON array: %s", encoded)
	}
	if decoded["errorMessage"] != "reranker timeout" {
		t.Fatalf("error message missing: %s", encoded)
	}
}

func TestRAGEvalAdminIdentityMatrix(t *testing.T) {
	tests := []struct {
		name     string
		identity auth.Identity
		want     bool
	}{
		{name: "super admin session", identity: auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session"}, want: true},
		{name: "anonymous", identity: auth.Identity{}},
		{name: "regular user session", identity: auth.Identity{UserID: "user", Role: users.RoleUser, AuthMethod: "session"}},
		{name: "admin API key", identity: auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "apikey", APIKeyType: users.APIKeyTypeAdmin}},
		{name: "act-as session", identity: auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session", ActAsUserID: "user"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRAGEvalAdminIdentity(tt.identity); got != tt.want {
				t.Fatalf("isRAGEvalAdminIdentity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMaskedDatasetVersionOmitsObjectKeyAndValidationReport(t *testing.T) {
	encoded, err := json.Marshal(maskEvalDatasetVersions([]store.RAGEvalDatasetVersionRecord{{ID: "v1", ManifestObjectKey: "secret/object/key", ValidationReportJSON: `{"private":true}`}}))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || json.Valid(encoded) == false {
		t.Fatal("masked DTO must be valid JSON")
	}
	var decoded []map[string]any
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded[0]["ManifestObjectKey"]; ok {
		t.Fatal("object key leaked")
	}
	if _, ok := decoded[0]["ValidationReportJSON"]; ok {
		t.Fatal("validation report leaked")
	}
}
