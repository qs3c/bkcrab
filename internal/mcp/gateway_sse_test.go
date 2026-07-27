package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGatewaySSEClientUsesMessageEndpoint(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/message" {
			t.Fatalf("path = %q, want /message", r.URL.Path)
		}
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		methods = append(methods, req.Method)
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"tools":[{"name":"fs_read","description":"read","inputSchema":{"type":"object"}}]}`)})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"file"}]}`)})
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer srv.Close()

	client := NewGatewaySSEClient(srv.URL+"/message", nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "fs_read" {
		t.Fatalf("tools = %#v", tools)
	}
	out, err := client.CallTool("fs_read", json.RawMessage(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if out != "file" {
		t.Fatalf("output = %q", out)
	}
	if len(methods) != 3 {
		t.Fatalf("method count = %d, want 3", len(methods))
	}
}
