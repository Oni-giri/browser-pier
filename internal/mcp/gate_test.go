package mcp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLargeRequestsAreServedAFewAtATime: requests over smallBody, or of
// unknown length, are served at most largeAtOnce at a time, while small
// ones and a short chunked one pass by.
func TestLargeRequestsAreServedAFewAtATime(t *testing.T) {
	var inFlight, most atomic.Int32
	var bodies sync.Map
	release := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		b, _ := io.ReadAll(r.Body)
		bodies.Store(r.Header.Get("X-Name"), len(b))
		if r.Header.Get("X-Hold") != "" {
			<-release
		}
	})
	h := gateLarge(make(chan struct{}, largeAtOnce), next)
	srv := httptest.NewServer(h)
	defer srv.Close()
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	defer letGo() // before srv.Close, which waits for the handlers

	big := bytes.Repeat([]byte("x"), smallBody+10)
	send := func(name string, body io.Reader, size int64, hold bool) error {
		req, _ := http.NewRequest("POST", srv.URL, body)
		req.ContentLength = size
		req.Header.Set("X-Name", name)
		if hold {
			req.Header.Set("X-Hold", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	var wg sync.WaitGroup
	for i, kind := range []string{"length", "length", "chunked", "length", "chunked"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var body io.Reader = bytes.NewReader(big)
			size := int64(len(big))
			if kind == "chunked" {
				body, size = io.MultiReader(bytes.NewReader(big)), -1 // hides the length: sent chunked
			}
			if err := send(kind+string(rune('0'+i)), body, size, true); err != nil {
				t.Error(err)
			}
		}()
	}
	// Five large requests: two get in and wait; small ones still pass.
	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < largeAtOnce && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := inFlight.Load(); n != largeAtOnce {
		t.Fatalf("%d large requests in at once, want %d", n, largeAtOnce)
	}
	if err := send("small", strings.NewReader("small"), 5, false); err != nil {
		t.Fatal(err)
	}
	if err := send("short chunked", io.MultiReader(strings.NewReader("short")), -1, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := bodies.Load("short chunked"); got != 5 {
		t.Errorf("the short chunked body reached the handler as %v bytes", got)
	}
	letGo()
	wg.Wait()
	if m := most.Load(); m > largeAtOnce+2 {
		t.Errorf("at most %d in flight, want no more than %d large and the 2 small", m, largeAtOnce)
	}
	n := 0
	bodies.Range(func(k, v any) bool {
		if k != "small" && k != "short chunked" {
			n++
			if v != len(big) {
				t.Errorf("%v reached the handler as %v bytes, want %d", k, v, len(big))
			}
		}
		return true
	})
	if n != 5 {
		t.Errorf("%d large requests served, want 5", n)
	}
}
