package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamableHTTPClientHandshakeAndTools(t *testing.T) {
	var sessionSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"protocolVersion":"2025-03-26"}`),
			})
		case "notifications/initialized":
			sessionSeen = r.Header.Get("Mcp-Session-Id")
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "sess-1" {
				t.Fatalf("tools/list missing session header")
			}
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"tools":[{"name":"time_now","description":"time","inputSchema":{"type":"object"}}]}`),
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"content":[{"type":"text","text":"noon"}]}`),
			})
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer srv.Close()

	client := NewStreamableHTTPClient(srv.URL, nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if sessionSeen != "sess-1" {
		t.Fatalf("initialized notification session = %q", sessionSeen)
	}
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "time_now" {
		t.Fatalf("tools = %#v", tools)
	}
	out, err := client.CallTool("time_now", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if out != "noon" {
		t.Fatalf("call output = %q", out)
	}
}

// TestStreamableHTTPClientConnectsWithoutSession proves the handshake no longer
// requires a Mcp-Session-Id response header. Stateless gateways (including
// lucky-aeon's aggregated /stream in some versions) omit it; the client must
// continue with an empty session instead of failing at connect.
func TestStreamableHTTPClientConnectsWithoutSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			// intentionally no Mcp-Session-Id header
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"protocolVersion":"2025-03-26"}`),
			})
		case "notifications/initialized":
			if r.Header.Get("Mcp-Session-Id") != "" {
				t.Fatalf("no session issued, client must not send Mcp-Session-Id")
			}
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"tools":[]}`),
			})
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer srv.Close()

	client := NewStreamableHTTPClient(srv.URL, nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect without session: %v", err)
	}
	if _, err := client.ListTools(); err != nil {
		t.Fatalf("list tools: %v", err)
	}
}
