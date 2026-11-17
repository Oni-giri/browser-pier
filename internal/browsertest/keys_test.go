package browsertest

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestKeySiteServesRecordsAndReports(t *testing.T) {
	s := NewKeySite(t, "127.0.0.1")
	get := func(u string) string {
		t.Helper()
		resp, err := http.Get(u) //nolint:gosec,noctx // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", u, resp.StatusCode)
		}
		return string(b)
	}
	post := func(path, body string) {
		t.Helper()
		resp, err := http.Post(s.URL+path, "application/json", strings.NewReader(body)) //nolint:gosec,noctx // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST %s: %d", path, resp.StatusCode)
		}
	}

	if page := get(s.Keys("1", 400, "field")); !strings.Contains(page, `id="f"`) || !strings.Contains(page, "autofocus") || !strings.Contains(page, "addEventListener('keydown'") {
		t.Errorf("focus=field: no autofocused field or no key listener:\n%s", page)
	}
	if page := get(s.Keys("2", 0, "body")); strings.Contains(page, "<input") || strings.Contains(page, "<iframe") {
		t.Errorf("focus=body has a field or a frame:\n%s", page)
	}
	framed := s.Framed("3", s.At("other.test")+`/keys?n=4&slow=400&focus=field"><script>`)
	if page := get(framed); !strings.Contains(page, `<iframe id="frame" title="Frame" src="http://other.test:`+s.Port+`/keys?n=4&amp;slow=400&amp;focus=field&#34;&gt;&lt;script&gt;"`) {
		t.Errorf("frame: the iframe isn't there, or its URL isn't escaped:\n%s", page)
	}
	if page := get(s.Arrived("5", s.Keys("6", 0, "field")+"&x=</script>")); !strings.Contains(page, "/arrived/report") || !strings.Contains(page, "prerendering") ||
		!strings.Contains(page, `next = "`+s.URL+`/keys?n=6\u0026slow=0\u0026focus=field\u0026x=\u003c/script\u003e"`) {
		t.Errorf("arrived page:\n%s", page)
	}

	start := time.Now()
	if page := get(Loading(s.Keys("7", 0, "field"), 300)); !strings.Contains(page, `id="f"`) || time.Since(start) < 300*time.Millisecond {
		t.Errorf("a loading page came after %v", time.Since(start))
	}

	post("/keys/report?n=1", `{"seq": 1, "ready": true, "focus": true, "active": "f", "keys": [], "value": ""}`)
	post("/keys/report?n=1", `{"seq": 3, "keys": [{"key": "b", "trusted": true, "t": 20}], "value": "ab"}`)
	// An older report that arrives late adds its keys, in the page's order,
	// but not its value.
	post("/keys/report?n=1", `{"seq": 2, "keys": [{"key": "Control", "ctrl": true, "trusted": true, "t": 10}], "value": "a"}`)
	r := s.Run("1")
	if !r.Ready || !r.Focus || r.Active != "f" || r.Value != "ab" || r.Arrived || len(r.Keys) != 2 || r.Keys[0].Key != "Control" || !r.Keys[0].Ctrl || r.Keys[1].Key != "b" {
		t.Errorf("run 1: %+v", r)
	}
	post("/arrived/report?n=5", "")
	if r, ok := s.WaitRun("5", time.Second, func(r KeyRun) bool { return r.Arrived }); !ok || r.Ready {
		t.Errorf("run 5: %+v", r)
	}
	s.Forget("1")
	if r := s.Run("1"); r.Ready || len(r.Keys) != 0 {
		t.Errorf("after Forget: %+v", r)
	}
}
