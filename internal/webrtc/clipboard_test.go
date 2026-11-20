package webrtc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// A paste is typed at instant pace and a copy reads the container's
// clipboard, both only for the viewer with control, and neither text is
// logged (PROJ-194).
func TestViewerPasteAndCopy(t *testing.T) {
	const pasted, copied = "pasted-Secret-42", "copied-Secret-43"
	var mu sync.Mutex
	var typed []string
	clip := copied
	var logs bytes.Buffer
	h, m, s := offerFixtureWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /input/type", func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			typed = append(typed, string(b))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("GET /input/clipboard", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			_, _ = io.WriteString(w, clip)
		})
	}, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	v := &viewer{h: h, session: s.ID, sock: m.Layout().ControlSocket("google")}
	send := func(msg string) []map[string]any {
		t.Helper()
		var got []map[string]any
		v.message([]byte(msg), func(r string) {
			var m map[string]any
			if err := json.Unmarshal([]byte(r), &m); err != nil {
				t.Fatalf("reply %q: %v", r, err)
			}
			got = append(got, m)
		})
		return got
	}
	paste := fmt.Sprintf(`{"type":"text","text":%q}`, pasted)

	// Without control: refused, nothing typed or read.
	for _, msg := range []string{paste, `{"type":"copy"}`} {
		if r := send(msg); len(r) != 1 || r[0]["type"] != "control" || r[0]["on"] != false {
			t.Errorf("%s without control: %v", msg, r)
		}
	}
	if len(typed) != 0 {
		t.Fatalf("typed without control: %v", typed)
	}

	if r := send(`{"type":"take"}`); len(r) != 1 || r[0]["on"] != true {
		t.Fatalf("take: %v", r)
	}
	if r := send(paste); len(r) != 0 {
		t.Errorf("paste: %v", r)
	}
	mu.Lock()
	if len(typed) != 1 || typed[0] != fmt.Sprintf(`{"pace":"instant","text":%q}`, pasted) {
		t.Errorf("typed %v", typed)
	}
	mu.Unlock()
	if r := send(`{"type":"copy"}`); len(r) != 1 || r[0]["type"] != "clipboard" || r[0]["text"] != copied {
		t.Errorf("copy: %v", r)
	}
	// An empty clipboard is an empty text; one over the cap is refused.
	mu.Lock()
	clip = ""
	mu.Unlock()
	if r := send(`{"type":"copy"}`); len(r) != 1 || r[0]["type"] != "clipboard" || r[0]["text"] != "" {
		t.Errorf("empty copy: %v", r)
	}
	mu.Lock()
	clip = strings.Repeat("a", maxClipboard+1)
	mu.Unlock()
	if r := send(`{"type":"copy"}`); len(r) != 1 || r[0]["type"] != "error" {
		t.Errorf("copy over the cap: %v", r)
	}

	// Neither text is logged, in the activity log or the orchestrator's.
	all := logs.String() + fmt.Sprint(m.Activity(s.ID))
	for _, secret := range []string{pasted, copied} {
		if strings.Contains(all, secret) {
			t.Errorf("%s is logged:\n%s", secret, all)
		}
	}
}
