package tools

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestWebFetchDeviceProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://example.com/docs" {
			t.Errorf("unexpected proxy target: %s", r.URL)
		}
		_, _ = io.WriteString(w, "proxied documentation")
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	req, _ := http.NewRequest("GET", "http://example.com/docs", nil)
	client, err := webFetchClient(req)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "proxied documentation" {
		t.Fatalf("unexpected body %s", body)
	}
	for _, raw := range []string{"http://127.0.0.1/", "http://localhost/", "http://10.0.0.1/", "http://[::1]/", "http://127.1/", "http://2130706433/", "http://user:pass@example.com/"} {
		next, _ := http.NewRequest("GET", raw, nil)
		if _, err := webFetchClient(next); err == nil {
			t.Errorf("accepted private target %s", raw)
		}
		if err := client.CheckRedirect(next, []*http.Request{req}); err == nil {
			t.Errorf("accepted redirect %s", raw)
		}
	}
}

func TestWebFetchNoProxyKeepsDirectGuard(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")
	t.Setenv("NO_PROXY", "example.com")
	req, _ := http.NewRequest("GET", "http://example.com/docs", nil)
	client, err := webFetchClient(req)
	if err != nil || client != safeFetchClient {
		t.Fatalf("direct guard not retained: %v", err)
	}
	for _, raw := range []string{"https://docs.python.org/3/", "https://github.com/python/cpython"} {
		u, _ := url.Parse(raw)
		if err := validateWebFetchProxyTarget(u); err != nil {
			t.Fatal(err)
		}
	}
}
