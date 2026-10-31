package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCloseBrowserSendsBrowserClose(t *testing.T) {
	got := make(chan string, 1)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://%s/devtools/browser/abc"}`, srv.Listener.Addr())
		case "/devtools/browser/abc":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			_, msg, err := conn.Read(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			got <- string(msg)
			// Chromium drops the connection as it closes rather than answering.
			_ = conn.CloseNow()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := closeBrowser(ctx, srv.Listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if msg := <-got; !strings.Contains(msg, `"method":"Browser.close"`) {
		t.Fatalf("sent %s", msg)
	}
}

func TestCloseBrowserFailsWithoutCDP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := closeBrowser(ctx, "127.0.0.1:1"); err == nil {
		t.Fatal("want an error when nothing listens")
	}
}

func TestCloseChromiumFallbackIsAnError(t *testing.T) {
	// With no CDP to answer, the SIGTERM fallback stops the process but may
	// have lost cookie writes, so it must not count as a clean close.
	p, err := spawn(exec.Command("sleep", "30"))
	if err != nil {
		t.Fatal(err)
	}
	err = closeChromium(p, "127.0.0.1:1", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "SIGTERM") {
		t.Fatalf("closeChromium = %v, want an error naming the SIGTERM fallback", err)
	}
	select {
	case <-p.done:
	default:
		t.Fatal("process still running after the fallback")
	}
}
