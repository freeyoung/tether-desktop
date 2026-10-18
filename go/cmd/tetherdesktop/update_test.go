package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpdateState(t *testing.T) {
	info := UpdateInfo{Latest: "0.4.0", MinSupported: "0.3.0"}
	for current, want := range map[string]UpdateState{
		"0.4.0": UpToDate, "0.5.0": UpToDate, "0.3.1": UpdateAvailable, "0.2.9": UpdateRequired,
	} {
		if got := info.State(current); got != want {
			t.Errorf("%s: %v, want %v", current, got, want)
		}
	}
}

func TestFetchUpdateInfo(t *testing.T) {
	var agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.UserAgent()
		_, _ = io.WriteString(w, `{"latest": "0.4.0", "min_supported": "0.3.0"}`)
	}))
	defer server.Close()
	info, err := FetchUpdateInfo(context.Background(), server.URL)
	if err != nil || info != (UpdateInfo{Latest: "0.4.0", MinSupported: "0.3.0"}) {
		t.Fatal(info, err)
	}
	if agent != "TetherDesktop/"+appVersion {
		t.Errorf("User-Agent %q", agent)
	}
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	if _, err := FetchUpdateInfo(context.Background(), missing.URL); err == nil {
		t.Fatal("no error for a 404")
	}
}
