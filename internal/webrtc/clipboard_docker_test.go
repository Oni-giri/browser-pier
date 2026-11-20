//go:build docker

package webrtc

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// TestRealBrowserPasteAndCopy is PROJ-194 on a real browser, through the
// live view's own input path and no CDP: a paste is typed into the
// address bar, then a copy (Ctrl+A, then the copy message and Ctrl+C, as
// the page sends them) brings the same text back from the browser's
// clipboard.
func TestRealBrowserPasteAndCopy(t *testing.T) {
	m, name := startBrowser(t)
	s, ok := m.Live(name)
	if !ok {
		t.Fatal("no live session")
	}
	relay, err := New(Options{Port: 0, IPs: []string{"127.0.0.1"}}, m.Layout().ControlSocket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()
	v := &viewer{h: NewHandler(relay, m, nil, slog.New(slog.DiscardHandler)), session: s.ID, sock: m.Layout().ControlSocket(name)}
	replies := make(chan map[string]any, 10)
	send := func(msg string) {
		t.Helper()
		v.message([]byte(msg), func(r string) {
			var got map[string]any
			_ = json.Unmarshal([]byte(r), &got)
			replies <- got
		})
	}
	next := func() map[string]any {
		t.Helper()
		select {
		case r := <-replies:
			if r["type"] == "error" || r["type"] == "copy-error" {
				t.Fatalf("%v", r)
			}
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("no reply")
			return nil
		}
	}
	input := func(evs ...map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"type": "input", "events": evs})
		send(string(b))
		time.Sleep(300 * time.Millisecond)
	}
	ctrl := func(k string) {
		input(map[string]any{"t": "keydown", "k": "Control_L"}, map[string]any{"t": "keydown", "k": k},
			map[string]any{"t": "keyup", "k": k}, map[string]any{"t": "keyup", "k": "Control_L"})
	}
	send(`{"type":"take"}`)
	if r := next(); r["on"] != true {
		t.Fatalf("take: %v", r)
	}
	// The window manager focuses on click: click the page first.
	input(map[string]any{"t": "move", "x": 640, "y": 500}, map[string]any{"t": "down", "b": 1}, map[string]any{"t": "up", "b": 1})
	const text = "héllo 123"
	ctrl("l") // the address bar
	send(`{"type":"text","text":"` + text + `"}`)
	time.Sleep(300 * time.Millisecond)
	ctrl("a")
	send(`{"type":"copy","id":7}`)
	ctrl("c")
	if r := next(); r["type"] != "clipboard" || r["id"] != float64(7) || r["text"] != text {
		t.Fatalf("copy: %v, want %q", r, text)
	}
	// Without control, a copy is refused.
	send(`{"type":"release"}`)
	<-replies
	send(`{"type":"copy","id":8}`)
	if r := <-replies; r["type"] != "control" || r["on"] != false {
		t.Errorf("copy without control: %v", r)
	}
}
