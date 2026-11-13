package pagetools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func (f *fixture) tabs(in TabsIn) (string, error) {
	out, err := f.tools.Tabs(context.Background(), f.session(), f.who, in)
	f.closedAfter()
	return out, err
}

// activated lists the targets Target.activateTarget was called with.
func (f *fixture) activated() []string {
	var out []string
	for _, m := range f.fb.Received() {
		if m.Method == "Target.activateTarget" {
			var p struct {
				TargetID string `json:"targetId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			out = append(out, p.TargetID)
		}
	}
	return out
}

func TestTabs(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/one", loginPage(), true)
	f.fb.addTab("https://example.com/two", loginPage(), false)
	out, err := f.tabs(TabsIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `t1 https://example.com/one "Page https://example.com/one" (active)`) || !strings.Contains(out, "t2 https://example.com/two") || strings.Contains(out, "sw.js") {
		t.Errorf("list:\n%s", out)
	}
	// switch brings the tab to the front.
	out, err = f.tabs(TabsIn{Action: "switch", ID: "t2"})
	if err != nil || !strings.Contains(out, "switched to t2; the active tab is now t2") || !strings.Contains(out, "URL: https://example.com/two") {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	if got := f.activated(); len(got) != 1 || got[0] != "T2" || f.fb.front != "T2" {
		t.Errorf("activated %v, front %s", got, f.fb.front)
	}
	if out := f.mustSnapshot(nil); !strings.Contains(out, "URL: https://example.com/two") {
		t.Errorf("the tools don't follow the switch:\n%s", out)
	}
	// new opens and brings to the front, with no notice for its own tab.
	out, err = f.tabs(TabsIn{Action: "new", URL: "https://example.com/three"})
	if err != nil || !strings.Contains(out, "opened t3") || strings.Contains(out, "a new tab opened") || f.fb.front != "T3" {
		t.Fatalf("new: %v\n%s", err, out)
	}
	if got := f.activated(); got[len(got)-1] != "T3" {
		t.Errorf("new didn't activate its tab: %v", got)
	}
	out, err = f.tabs(TabsIn{Action: "new"})
	if err != nil || !strings.Contains(out, "URL: about:blank") {
		t.Fatalf("new without a URL: %v\n%s", err, out)
	}
	for _, bad := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://u:p@example.com/"} {
		if _, err := f.tabs(TabsIn{Action: "new", URL: bad}); !isStatus(err, 400) {
			t.Errorf("new %s: %v", bad, err)
		}
	}
	// Closing the active tab (t4) brings back the one in front before it (t3).
	out, err = f.tabs(TabsIn{Action: "close"})
	if err != nil || !strings.Contains(out, "closed t4; the active tab is now t3") || f.fb.front != "T3" {
		t.Fatalf("close the active tab: %v\n%s (front %s)", err, out, f.fb.front)
	}
	// Closing another leaves the active one alone.
	if out, err := f.tabs(TabsIn{Action: "close", ID: "t1"}); err != nil || !strings.Contains(out, "the active tab is now t3") {
		t.Errorf("close t1: %v\n%s", err, out)
	}
	if _, err := f.tabs(TabsIn{Action: "switch", ID: "t1"}); !isStatus(err, http.StatusNotFound) {
		t.Errorf("switch to a closed tab: %v", err)
	}
	if _, err := f.tabs(TabsIn{Action: "close", ID: "t2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tabs(TabsIn{Action: "close"}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "last tab") {
		t.Errorf("close the last tab: %v", err)
	}
	if _, err := f.tabs(TabsIn{Action: "fly"}); !isStatus(err, 400) {
		t.Errorf("unknown action: %v", err)
	}
	checkSent(t, f.fb)
}

func TestTabsListIsCapped(t *testing.T) {
	f := newFixture(t)
	for i := range 25 {
		f.fb.addTab(fmt.Sprintf("https://example.com/%d", i), loginPage(), i == 0)
	}
	out, err := f.tabs(TabsIn{})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "\nt"); n != maxTabsListed || !strings.Contains(out, "…and 5 more tabs.") {
		t.Errorf("%d tabs listed:\n%s", n, out)
	}
}

func TestClosingAPopupBringsBackItsOpener(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.mustSnapshot(nil)
	f.fb.addTab("https://example.com/background", loginPage(), false) // a target=_blank link
	f.fb.addTab("https://accounts.example/", loginPage(), true)       // a popup, in front
	out := f.mustSnapshot(nil)
	if !strings.Contains(out, "a new tab opened: t2") || !strings.Contains(out, "a new tab opened: t3") || !strings.Contains(out, "URL: https://accounts.example/") {
		t.Fatalf("after the popup:\n%s", out)
	}
	out, err := f.tabs(TabsIn{Action: "close"})
	if err != nil || !strings.Contains(out, "closed t3; the active tab is now t1") || f.fb.front != "T1" {
		t.Fatalf("%v\n%s", err, out)
	}
}
