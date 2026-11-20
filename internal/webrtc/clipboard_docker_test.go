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
// address bar, then Ctrl+A, Ctrl+C and a copy bring the same text back
// from the browser's clipboard.
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
	send := func(msg string) map[string]any {
		t.Helper()
		var got map[string]any
		v.message([]byte(msg), func(r string) { _ = json.Unmarshal([]byte(r), &got) })
		if got != nil && got["type"] == "error" {
			t.Fatalf("%s: %v", msg, got)
		}
		return got
	}
	// keys presses ctrl with each key in turn, as the live view sends them.
	keys := func(ks ...string) {
		t.Helper()
		var evs []map[string]string
		for _, k := range ks {
			evs = append(evs, map[string]string{"t": "keydown", "k": "Control_L"}, map[string]string{"t": "keydown", "k": k},
				map[string]string{"t": "keyup", "k": k}, map[string]string{"t": "keyup", "k": "Control_L"})
		}
		b, _ := json.Marshal(map[string]any{"type": "input", "events": evs})
		send(string(b))
		time.Sleep(300 * time.Millisecond)
	}
	if r := send(`{"type":"take"}`); r["on"] != true {
		t.Fatalf("take: %v", r)
	}
	const text = "héllo 123"
	keys("l") // the address bar
	send(`{"type":"text","text":"` + text + `"}`)
	time.Sleep(300 * time.Millisecond)
	keys("a", "c")
	var got string
	for try := 0; try < 10 && got != text; try++ {
		r := send(`{"type":"copy"}`)
		got, _ = r["text"].(string)
		if got != text {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if got != text {
		t.Fatalf("copied %q, want %q", got, text)
	}
	// Without control, a copy is refused.
	send(`{"type":"release"}`)
	if r := send(`{"type":"copy"}`); r["type"] != "control" || r["on"] != false || r["text"] != nil {
		t.Errorf("copy without control: %v", r)
	}
}
