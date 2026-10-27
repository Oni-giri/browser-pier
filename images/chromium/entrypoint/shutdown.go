package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
)

// closeBrowser asks Chromium to close through CDP's Browser.close, the same
// path as closing its last window. Unlike SIGTERM, which exits before the
// network service flushes the cookie store, this shuts the profile down
// properly, so a cookie set a moment ago reaches disk. It returns once
// Chromium has the request; the caller waits for the process to exit.
func closeBrowser(ctx context.Context, cdp string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cdp+"/json/version", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return fmt.Errorf("CDP version: %w", err)
	}

	conn, _, err := websocket.Dial(ctx, version.WebSocketDebuggerURL, nil) //nolint:bodyclose // the connection owns the body
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Browser.close"}`)); err != nil {
		return err
	}
	// Chromium answers, or drops the connection as it closes; either means it has the request.
	_, _, _ = conn.Read(ctx)
	return nil
}
