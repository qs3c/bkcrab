package agent

import (
	"context"
	"testing"

	agenttools "github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
)

type managerRAGStub struct {
	refs  []agenttools.RAGKBRef
	calls int
}

func (*managerRAGStub) SearchForAgent(context.Context, string, []string, string, int) (string, error) {
	return "", nil
}

func (s *managerRAGStub) ResolveAgentKBs(context.Context, string, []string) []agenttools.RAGKBRef {
	s.calls++
	return append([]agenttools.RAGKBRef(nil), s.refs...)
}

func managerHasTool(ag *Agent, name string) bool {
	for _, tool := range ag.RegisteredTools() {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// manager RAG registration cases
var managerRAGCases = []struct {
	name      string
	rag       config.RAGAgentCfg
	withSvc   bool
	refs      []agenttools.RAGKBRef
	wantTool  bool
	wantCalls int
}{
	{
		name:    "agent has no RAG configuration",
		withSvc: true,
		refs:    []agenttools.RAGKBRef{{ID: "kb-1", Name: "Manual"}},
	},
	{
		name: "RAG service is unavailable",
		rag:  config.RAGAgentCfg{KBs: []string{"kb-1"}},
	},
	{
		name:      "configured KB is not authorized",
		rag:       config.RAGAgentCfg{KBs: []string{"kb-1"}},
		withSvc:   true,
		wantCalls: 1,
	},
	{
		name:      "configured KB and service are available",
		rag:       config.RAGAgentCfg{KBs: []string{"kb-1"}, TopN: 7},
		withSvc:   true,
		refs:      []agenttools.RAGKBRef{{ID: "kb-1", Name: "Manual"}},
		wantTool:  true,
		wantCalls: 1,
	},
}

func TestManagerConditionallyRegistersRAGSearch(t *testing.T) {
	for _, tt := range managerRAGCases {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("BKCRAB_HOME", home)
			rc := config.ResolvedAgent{
				ID:        "agent-rag",
				UserID:    "agent-owner",
				Model:     "provider/model",
				Home:      home,
				Workspace: t.TempDir(),
				RAG:       tt.rag,
			}
			stub := &managerRAGStub{refs: tt.refs}
			opts := []ManagerOption{WithUserID("manager-user")}
			if tt.withSvc {
				opts = append(opts, WithRAGService(stub))
			}
			mgr, err := NewManager([]config.ResolvedAgent{rc}, nil, bus.New(), opts...)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			got := managerHasTool(mgr.AgentByID(rc.ID), "rag_search")
			if got != tt.wantTool {
				t.Fatalf("rag_search registered = %v, want %v", got, tt.wantTool)
			}
			if stub.calls != tt.wantCalls {
				t.Fatalf("ResolveAgentKBs calls = %d, want %d", stub.calls, tt.wantCalls)
			}
			if tt.wantTool {
				visible := false
				for _, definition := range mgr.AgentByID(rc.ID).registry.DefinitionsForMode(builtinAllowForMode(config.PromptModeChatbot)) {
					if definition.Function.Name == "rag_search" {
						visible = true
						break
					}
				}
				if !visible {
					t.Fatal("registered rag_search is hidden from chatbot prompt mode")
				}
			}
		})
	}
}

func TestRAGSearchSDKMetadataIsReadOnly(t *testing.T) {
	adapter := &toolAdapter{name: "rag_search"}
	if !adapter.IsReadOnly(nil) || !adapter.IsConcurrencySafe(nil) {
		t.Fatal("rag_search must be marked read-only and concurrency-safe")
	}
}
