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
	"time"
)

// A paste goes to the container's paste and a copy reads its
// clipboard, both only for the viewer with control, and neither text is
// logged (PROJ-194).
func TestViewerPasteAndCopy(t *testing.T) {
	const pasted, copied = "pasted-Secret-42", "copied-Secret-43"
	var mu sync.Mutex
	var typed []string
	clip := copied
	var logs bytes.Buffer
	h, m, s := offerFixtureWith(t, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /input/paste", func(w http.ResponseWriter, r *http.Request) {
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
	copySettle, copyPoll = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { copySettle, copyPoll = 1500*time.Millisecond, 100*time.Millisecond })
	replies := make(chan map[string]any, 10)
	send := func(msg string) []map[string]any {
		t.Helper()
		v.message([]byte(msg), func(r string) {
			var m map[string]any
			if err := json.Unmarshal([]byte(r), &m); err != nil {
				t.Errorf("reply %q: %v", r, err)
			}
			replies <- m
		})
		var got []map[string]any
		for len(replies) > 0 {
			got = append(got, <-replies)
		}
		return got
	}
	next := func() map[string]any {
		t.Helper()
		select {
		case r := <-replies:
			return r
		case <-time.After(3 * time.Second):
			t.Fatal("no reply")
			return nil
		}
	}
	setClip := func(c string) { mu.Lock(); clip = c; mu.Unlock() }
	paste := fmt.Sprintf(`{"type":"text","text":%q}`, pasted)

	// Without control: refused, nothing typed or read.
	for _, msg := range []string{paste, `{"type":"copy","id":1}`} {
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
	if len(typed) != 1 || typed[0] != fmt.Sprintf(`{"text":%q}`, pasted) {
		t.Errorf("typed %v", typed)
	}
	mu.Unlock()

	// A copy answers once the clipboard changes: the copy keys come after
	// the message, so what it held before isn't the answer.
	setClip("an older copy")
	if r := send(`{"type":"copy","id":2}`); len(r) != 0 {
		t.Fatalf("copy answered before the keys: %v", r)
	}
	time.Sleep(50 * time.Millisecond)
	setClip(copied)
	if r := next(); r["type"] != "clipboard" || r["id"] != float64(2) || r["text"] != copied {
		t.Errorf("copy: %v", r)
	}
	// Copying the same text again: answered once the wait is over.
	start := time.Now()
	send(`{"type":"copy","id":3}`)
	if r := next(); r["type"] != "clipboard" || r["id"] != float64(3) || r["text"] != copied || time.Since(start) < copySettle {
		t.Errorf("unchanged copy: %v after %v", r, time.Since(start))
	}
	// An empty clipboard is an empty text; one over the cap is an error.
	setClip("")
	send(`{"type":"copy","id":4}`)
	if r := next(); r["type"] != "clipboard" || r["text"] != "" {
		t.Errorf("empty copy: %v", r)
	}
	setClip(strings.Repeat("a", maxClipboard+1))
	if r := send(`{"type":"copy","id":5}`); len(r) != 1 || r[0]["type"] != "copy-error" || r[0]["id"] != float64(5) {
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
