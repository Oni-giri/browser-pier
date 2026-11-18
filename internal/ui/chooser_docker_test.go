//go:build docker

// The file chooser modal (PROJ-181) in a real browser: on the handoff
// page at a phone's size, and in the live view at a desktop's. The
// profile's browser is a stand-in with a fake portal (chooser_test.go);
// internal/api's TestChooserDuringHandoff answers a real page's chooser
// through the same routes. See internal/sessions/docker_test.go for the
// environment it needs.
package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

// shoot saves what the browser shows as name.png in
// BROWSERPIER_TEST_SCREENSHOTS, if that is set.
func shoot(t *testing.T, b *browsertest.CDP, name string) {
	t.Helper()
	dir := os.Getenv("BROWSERPIER_TEST_SCREENSHOTS")
	if dir == "" {
		return
	}
	time.Sleep(300 * time.Millisecond) // a theme change repaints
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(b.Call("Page.captureScreenshot", map[string]any{"format": "png"}), &out); err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
}

func scheme(b *browsertest.CDP, s string) {
	b.Call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": s}}})
}

// TestChooserModal: while the person has control and a page's chooser
// is open, the modal shows the profile's uploads, their names as text;
// it fits a phone; Esc leaves it open; "Upload from this device" stores
// the file and answers the chooser with it; in the live view, several
// uploads answer a chooser that takes several.
func TestChooserModal(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	f := newFixture(t)
	p := profiles.New("google")
	p.HandoffPolicy = profiles.PolicyLink
	if _, err := f.m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	s, _, err := f.m.Start(ctx, "google", "")
	if err != nil {
		t.Fatal(err)
	}
	marked := "cv <b>2026.pdf"
	f.putUpload("google", "older.pdf", "%PDF old")
	old := time.Now().Add(-3 * time.Hour)
	_ = os.Chtimes(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "older.pdf"), old, old)
	f.putUpload("google", marked, "%PDF cv")
	f.choosers.set(`[{"id":"c1","multiple":false,"accept":[],"left_ms":240000}]`)
	h, token, err := f.handoffs.Create(ctx, s.ID, "claude", handoff.FileUpload, "Upload your CV to the form", "")
	if err != nil {
		t.Fatal(err)
	}

	phone, container := newPhoneContainer(t, image)
	// The live view's data channel, which Take control needs, connects
	// through a TURN server where the phone reaches this machine, as in
	// TestHandoffLiveViewThroughTURN.
	ip := phoneSeesHostAt(t, container)
	var key [16]byte
	_, _ = rand.Read(key[:])
	secret := hex.EncodeToString(key[:])
	addr := browsertest.Coturn(t, ip, ip, secret)
	turn := webrtc.TURN{URLs: []string{"turn:" + addr + "?transport=udp", "turn:" + addr + "?transport=tcp"}, Secret: secret}
	relay, err := webrtc.New(webrtc.Options{Port: 0, IPs: []string{ip}, TURN: turn}, f.m.Layout().ControlSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	mux := http.NewServeMux()
	live := webrtc.NewHandler(relay, f.m, f.web, log)
	live.Register(mux)
	hw := handoff.NewWeb(f.handoffs, f.web, live, f.m, log)
	hw.SetPicker(f.picker)
	hw.Register(mux)
	mux.Handle("/", f.srv.Config.Handler)
	base := serveToBrowser(t, mux)
	open := `(() => { const d = document.getElementById("chooser"); return !!d && d.open; })()`

	// The phone, on the handoff link: the live view takes control, and
	// the modal comes up for the open chooser.
	phone.Call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 2, "mobile": true})
	scheme(phone, "light")
	phone.Navigate(base + "/h/" + token)
	untilFor(t, phone, 45*time.Second, "the modal never opened on the phone", open)
	is(t, phone, `[...document.querySelectorAll("#chooser .chooser-name > span:first-child")].map((e) => e.textContent).join("|")`, marked+"|older.pdf")
	is(t, phone, `document.querySelectorAll("#chooser b, #chooser input[type=checkbox]").length`, float64(0))
	is(t, phone, `document.querySelector("#chooser h2").textContent`, "Choose a file for the page")
	is(t, phone, `document.querySelector("#chooser [type=submit]").disabled`, true)
	m, _ := phone.Eval(`(() => {
		const d = document.getElementById("chooser").getBoundingClientRect();
		const up = document.querySelector("#chooser [data-upload]").getBoundingClientRect();
		const cancel = document.querySelector("#chooser [data-cancel]").getBoundingClientRect();
		const choose = document.querySelector("#chooser [type=submit]").getBoundingClientRect();
		return [document.documentElement.scrollWidth, innerWidth, innerHeight, d.left, d.right, d.top, d.bottom, up.width, d.width, cancel.height, cancel.top, choose.top, up.bottom];
	})()`).([]any)
	if len(m) != 13 {
		t.Fatalf("measuring the modal: %v", m)
	}
	v := make([]float64, len(m))
	for i := range m {
		v[i] = m[i].(float64)
	}
	scroll, width, height := v[0], v[1], v[2]
	if scroll > width || v[3] < 0 || v[4] > width || v[5] < 0 || v[6] > height {
		t.Fatalf("on a %v×%v phone the page is %v wide and the modal is at x %v–%v, y %v–%v", width, height, scroll, v[3], v[4], v[5], v[6])
	}
	// Upload on its own row, nearly the modal's width; Cancel and Choose
	// side by side under it, big enough for a thumb.
	if v[7] < v[8]-48 || v[9] < 44 || v[10] != v[11] || v[10] < v[12] {
		t.Fatalf("the phone's buttons: upload %v wide in a %v modal, cancel %v tall at %v, choose at %v, upload ends at %v", v[7], v[8], v[9], v[10], v[11], v[12])
	}
	shoot(t, phone, "chooser-handoff-light-phone")
	scheme(phone, "dark")
	shoot(t, phone, "chooser-handoff-dark-phone")
	scheme(phone, "light")

	// Esc leaves it open: the page waits for an answer.
	phone.Call("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "Escape", "code": "Escape", "windowsVirtualKeyCode": 27})
	phone.Call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Escape", "code": "Escape", "windowsVirtualKeyCode": 27})
	time.Sleep(500 * time.Millisecond)
	is(t, phone, open, true)

	// Upload from this device: a photo from the phone answers the chooser.
	photo := "a photo from the phone"
	if out, err := exec.Command("docker", "exec", container, "sh", "-c", "printf '"+photo+"' > /tmp/IMG_2207.jpg").CombinedOutput(); err != nil {
		t.Fatalf("putting a file on the phone: %v %s", err, out)
	}
	var doc struct {
		Root struct {
			NodeID int `json:"nodeId"`
		} `json:"root"`
	}
	_ = json.Unmarshal(phone.Call("DOM.getDocument", map[string]any{"depth": 0}), &doc)
	var input struct {
		NodeID int `json:"nodeId"`
	}
	_ = json.Unmarshal(phone.Call("DOM.querySelector", map[string]any{"nodeId": doc.Root.NodeID, "selector": "#chooser [data-device]"}), &input)
	phone.Call("DOM.setFileInputFiles", map[string]any{"files": []string{"/tmp/IMG_2207.jpg"}, "nodeId": input.NodeID})
	until(t, phone, "the modal didn't close after the upload", `!document.getElementById("chooser").open`)
	if got := f.choosers.got(); len(got) != 1 || got[0] != filesJSON("IMG_2207.jpg") {
		t.Fatalf("the browser got %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "IMG_2207.jpg")); err != nil || string(b) != photo {
		t.Fatalf("the upload: %q %v", b, err)
	}

	// Handed back; then the admin, in the live view at a desktop's size,
	// takes control, and a chooser that takes several opens.
	if _, err := f.handoffs.Finish(ctx, h, handoff.Done, ""); err != nil {
		t.Fatal(err)
	}
	f.choosers.set(`[{"id":"c2","multiple":true,"accept":[],"left_ms":300000}]`)
	desk := phone
	desk.Call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1440, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	desk.Navigate(base + "/login")
	desk.Eval(`document.querySelector("#password").value = "the-admin-password"; document.querySelector("#password").form.requestSubmit()`)
	until(t, desk, "signing in didn't leave the login page", `location.pathname === "/"`)
	desk.Navigate(base + "/profiles/google")
	for deadline := time.Now().Add(45 * time.Second); desk.Eval(`!document.getElementById("take").disabled && document.getElementById("take").textContent === "Take control"`) != true; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("Take control never became available: %v", desk.Eval(`JSON.stringify([location.href, document.getElementById("take").textContent, document.getElementById("take").disabled, document.getElementById("who").textContent])`))
		}
	}
	desk.Eval(`document.getElementById("take").click()`)
	untilFor(t, desk, 15*time.Second, "the modal never opened in the live view", open)
	is(t, desk, `document.querySelectorAll("#chooser input[type=checkbox]").length`, float64(3))
	is(t, desk, `document.querySelector("#chooser h2").textContent`, "Choose files for the page")
	desk.Eval(`document.querySelectorAll("#chooser input[type=checkbox]")[0].click(); document.querySelectorAll("#chooser input[type=checkbox]")[1].click()`)
	is(t, desk, `document.querySelector("#chooser [type=submit]").textContent`, "Choose 2 files")
	shoot(t, desk, "chooser-live-light-wide")
	scheme(desk, "dark")
	shoot(t, desk, "chooser-live-dark-wide")
	// In the live view's dark theme the dialog's Choose is its primary:
	// light on dark, as DESIGN.md has it.
	if bg := fmt.Sprint(desk.Eval(`getComputedStyle(document.querySelector("#chooser [type=submit]")).backgroundColor`)); bg != "rgb(232, 237, 245)" {
		t.Errorf("Choose in dark is %s, want the light primary", bg)
	}
	scheme(desk, "light")
	names := strings.Split(fmt.Sprint(desk.Eval(`[...document.querySelectorAll("#chooser input:checked")].map((i) => i.value).join("|")`)), "|")
	desk.Eval(`document.querySelector("#chooser [type=submit]").click()`)
	until(t, desk, "the modal didn't close after Choose", `!document.getElementById("chooser").open`)
	if got := f.choosers.got(); len(got) != 1 || got[0] != filesJSON(names...) {
		t.Fatalf("the browser got %q, want %s", got, filesJSON(names...))
	}
	var entries []string
	for _, a := range f.m.Activity(s.ID) {
		entries = append(entries, a.What)
	}
	if all := strings.Join(entries, "\n"); !strings.Contains(all, "A person chose 1 file") || !strings.Contains(all, "A person chose 2 files") || strings.Contains(all, "IMG_2207") {
		t.Fatalf("activity: %q", entries)
	}
}
