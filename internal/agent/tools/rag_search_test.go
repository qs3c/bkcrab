package tools

import (
	"context"
	"strings"
	"testing"
)

type fakeRAGSearcher struct {
	owner string
	kbs   []string
	query string
	topN  int
}

func (f *fakeRAGSearcher) SearchForAgent(_ context.Context, ownerID string, kbIDs []string, query string, topN int) (string, error) {
	f.owner = ownerID
	f.kbs = append([]string(nil), kbIDs...)
	f.query = query
	f.topN = topN
	return "[来源: a.md · chunk#0 · 知识库:手册]\n内容", nil
}

func TestRegisterRAGSearchSkipsWhenNoKBs(t *testing.T) {
	r := NewRegistry("", "")
	RegisterRAGSearch(r, &fakeRAGSearcher{}, "u1", nil, 0)
	for _, tool := range r.RegisteredTools() {
		if tool.Name == "rag_search" {
			t.Fatal("无授权 KB 不应注册 rag_search")
		}
	}
}

func TestRAGSearchListsKBsAndSearches(t *testing.T) {
	r := NewRegistry("", "")
	f := &fakeRAGSearcher{}
	RegisterRAGSearch(r, f, "u1", []RAGKBRef{{ID: "kb_1", Name: "产品手册", Description: "产品相关"}}, 0)

	var found bool
	for _, tool := range r.RegisteredTools() {
		if tool.Name == "rag_search" {
			found = true
			if !strings.Contains(tool.Description, "产品手册") {
				t.Fatalf("工具描述未列出授权 KB: %q", tool.Description)
			}
		}
	}
	if !found {
		t.Fatal("rag_search 未注册")
	}

	out, err := r.Execute(context.Background(), "rag_search", `{"query":"如何安装"}`)
	if err != nil || !strings.Contains(out, "[来源:") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if f.owner != "u1" || len(f.kbs) != 1 || f.kbs[0] != "kb_1" || f.topN != 5 {
		t.Fatalf("调用参数错误: owner=%q kbs=%v topN=%d", f.owner, f.kbs, f.topN)
	}

	_, err = r.Execute(context.Background(), "rag_search", `{"query":"x","kb":"不存在"}`)
	if err == nil || !strings.Contains(err.Error(), "产品手册") {
		t.Fatalf("未知 KB 应返回可用名单: %v", err)
	}

	turn := r.ForTurn()
	if _, err := turn.Execute(context.Background(), "rag_search", `{"query":"x","top_n":99}`); err != nil {
		t.Fatal(err)
	}
	if f.topN != 20 {
		t.Fatalf("top_n 应限制到 20, got %d", f.topN)
	}
}
