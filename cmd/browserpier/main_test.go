package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	h := newHandler()
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodHead, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.code {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.code)
		}
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, newHandler(), slog.New(slog.DiscardHandler)) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

func TestDefaultBrowserImage(t *testing.T) {
	for version, want := range map[string]string{
		"v1.2.3":              "ghcr.io/oni-giri/browser-pier-chromium:1.2.3",
		"v1.2.3-4-gabcdef0":   "ghcr.io/oni-giri/browser-pier-chromium:edge",
		"dev":                 "ghcr.io/oni-giri/browser-pier-chromium:edge",
		"abcdef0-dirty":       "ghcr.io/oni-giri/browser-pier-chromium:edge",
		"v1.2.3\nevil:latest": "ghcr.io/oni-giri/browser-pier-chromium:edge",
	} {
		if got := defaultBrowserImage(version); got != want {
			t.Errorf("defaultBrowserImage(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestBrowserUser(t *testing.T) {
	if u, g := browserUser(1001, 1002); u != 1001 || g != 1002 {
		t.Errorf("browserUser(1001, 1002) = %d, %d", u, g)
	}
	if u, g := browserUser(0, 0); u != 1000 || g != 1000 {
		t.Errorf("browserUser(0, 0) = %d, %d; Chromium can't run as root", u, g)
	}
}
