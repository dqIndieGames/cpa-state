package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeaderWaitOwnedByClient(t *testing.T) {
	var calls atomic.Int32
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		select {
		case <-time.After(76 * time.Second):
			_, _ = io.WriteString(w, "headers-arrived")
		case <-req.Context().Done():
			close(cancelled)
		}
	}))
	defer upstream.Close()
	r := testRelay(t)
	r.upstream = upstream.URL
	server := httptest.NewServer(r.handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/responses", strings.NewReader(`{"model":"test"}`))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "headers-arrived" {
		t.Fatalf("relay interrupted delayed headers: status=%d body=%q", response.StatusCode, body)
	}
	if calls.Load() != 1 {
		t.Fatal("relay repeated a client request")
	}
}
