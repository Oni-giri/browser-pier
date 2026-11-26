//go:build docker

package sessions

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/notify"
)

// TestABrowserThatDiesTellsTheWebhook: with browser.failed on, a browser
// container killed under a running session sends one signed event, with
// the category crashed and nothing from the container's log.
func TestABrowserThatDiesTellsTheWebhook(t *testing.T) {
	f := newRealFixture(t)
	m := f.manager(f.layout)
	name := f.profile(m)

	var mu sync.Mutex
	var bodies []string
	var sigs []string
	rc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		sigs = append(sigs, r.Header.Get(notify.SignatureHeader))
		mu.Unlock()
	}))
	defer rc.Close()
	n, err := notify.New(context.Background(), m.db, slog.New(slog.NewTextHandler(testWriter{t}, nil)), notify.Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := n.Save(context.Background(), notify.Change{URL: rc.URL + "/hook", On: map[notify.Kind]bool{notify.BrowserFailed: true}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	m.OnFailed(func(s Session, how EndReason) { n.BrowserFailed(s.Profile, string(how)) })

	s := startRunning(t, m, name)
	if out, err := exec.Command("docker", "kill", s.ContainerID).CombinedOutput(); err != nil {
		t.Fatalf("docker kill: %v %s", err, out)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		mu.Lock()
		got := len(bodies)
		mu.Unlock()
		if got > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the webhook got nothing")
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("the webhook got %d events: %v", len(bodies), bodies)
	}
	b := bodies[0]
	if !strings.Contains(b, `"event":"browser.failed"`) || !strings.Contains(b, `"category":"crashed"`) || !strings.Contains(b, `"profile":"`+name+`"`) {
		t.Errorf("body: %s", b)
	}
	for _, bad := range []string{"level=", "exit status", "msg=", "err="} {
		if strings.Contains(b, bad) {
			t.Errorf("the body holds %q: %s", bad, b)
		}
	}
	ts, _, _ := strings.Cut(strings.TrimPrefix(sigs[0], "t="), ",")
	var at int64
	for _, c := range ts {
		at = at*10 + int64(c-'0')
	}
	if notify.Sign(secret, at, []byte(b)) != sigs[0] {
		t.Error("the signature doesn't verify")
	}
	if got, err := m.LastSession(context.Background(), name); err != nil || got.State != Failed {
		t.Errorf("the session: %+v, %v", got, err)
	}
}
