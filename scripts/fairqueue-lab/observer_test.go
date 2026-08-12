package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestDeleteRabbitQueueIsBoundedToEmptyUnusedQueue(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gotPath, gotQuery = request.URL.Path, request.URL.RawQuery
		user, password, ok := request.BasicAuth()
		if request.Method != http.MethodDelete || !ok || user != "lab" || password != "secret" {
			t.Fatalf("request method/auth = %s %t %q %q", request.Method, ok, user, password)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	observer := &labObserver{
		rabbitURL: base, rabbitUser: "lab", rabbitPass: "secret", rabbitVHost: "bkcrab",
		rabbitHTTP: server.Client(),
	}
	if err := observer.deleteRabbitQueue(context.Background(), "bkcrab.fair.q.rag.index.abc"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/queues/bkcrab/bkcrab.fair.q.rag.index.abc" {
		t.Fatalf("path=%q", gotPath)
	}
	query, err := url.ParseQuery(gotQuery)
	if err != nil || query.Get("if-empty") != "true" || query.Get("if-unused") != "true" {
		t.Fatalf("query=%q parsed=%v err=%v", gotQuery, query, err)
	}
}

func TestDeleteRabbitQueueTreatsAlreadyAbsentAsClean(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	observer := &labObserver{rabbitURL: base, rabbitHTTP: server.Client()}
	if err := observer.deleteRabbitQueue(context.Background(), "bkcrab.fair.q.rag.index.absent"); err != nil {
		t.Fatal(err)
	}
}
