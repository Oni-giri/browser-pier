//go:build docker

// The live view's script through the bundled TURN server, in a real
// browser; see internal/sessions/docker_test.go for the environment it
// needs.
package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

// TestHandoffLiveViewThroughTURN: a handoff link opened with ?ice=relay
// connects its live view only through the bundled TURN server, with the
// credentials the page fetched for itself.
func TestHandoffLiveViewThroughTURN(t *testing.T) {
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
	_, token, err := f.handoffs.Create(ctx, s.ID, "claude", handoff.SensitiveInput, "Enter the 2FA code", "")
	if err != nil {
		t.Fatal(err)
	}

	phone, container := newPhoneContainer(t, image)
	// coturn and the relay listen where the phone reaches this machine.
	ip := phoneSeesHostAt(t, container)
	var b [32]byte
	_, _ = rand.Read(b[:])
	secret := hex.EncodeToString(b[:])
	addr := browsertest.Coturn(t, ip, ip, secret)
	// A third TURN server the phone can't reach (TEST-NET, which goes
	// nowhere), as one a network blocks: the page mustn't wait it out.
	turn := webrtc.TURN{URLs: []string{"turn:" + addr + "?transport=udp", "turn:" + addr + "?transport=tcp", "turn:192.0.2.1:3478?transport=tcp"}, Secret: secret}
	relay, err := webrtc.New(webrtc.Options{Port: 0, IPs: []string{ip}, TURN: turn}, f.m.Layout().ControlSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	mux := http.NewServeMux()
	live := webrtc.NewHandler(relay, f.m, f.web, log)
	live.Register(mux) // live.js
	handoff.NewWeb(f.handoffs, f.web, live, f.m, log).Register(mux)
	mux.Handle("/", f.srv.Config.Handler)
	link := serveToBrowser(t, mux) + "/h/" + token

	// Keep each RTCPeerConnection the page makes, and what it was made with.
	phone.Call("Page.enable", nil)
	phone.Call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `(() => {
		const Real = RTCPeerConnection;
		window.__pcs = [];
		window.RTCPeerConnection = function (cfg) {
			const pc = new Real(cfg), rec = { pc, cfg, made: performance.now() };
			pc.addEventListener("connectionstatechange", () => { if (pc.connectionState === "connected") rec.ms = Math.round(performance.now() - rec.made); });
			window.__pcs.push(rec);
			return pc;
		};
		window.RTCPeerConnection.prototype = Real.prototype;
	})()`})
	phone.Navigate(link) // opens the link here; it then loads without the query
	phone.Navigate(link + "?ice=relay")

	state := func() string {
		return fmt.Sprint(phone.Eval(`window.__pcs && __pcs.length ? __pcs[0].pc.connectionState : "none"`))
	}
	for deadline := time.Now().Add(45 * time.Second); state() != "connected"; time.Sleep(250 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the live view never connected through TURN: %s (ICE %v); the page says %v", state(),
				phone.Eval(`window.__pcs && __pcs.length ? __pcs[0].pc.iceConnectionState + ", gathering " + __pcs[0].pc.iceGatheringState : "no connection made"`),
				phone.Eval(`document.getElementById("who").textContent`))
		}
	}
	cfg := fmt.Sprint(phone.Eval(`JSON.stringify(__pcs[0].cfg)`))
	if !strings.Contains(cfg, `"iceTransportPolicy":"relay"`) || !strings.Contains(cfg, `"credential":"`) || !strings.Contains(cfg, addr) {
		t.Fatalf("the page connected with %s", cfg)
	}
	if strings.Contains(cfg, secret) {
		t.Fatal("the shared secret reached the page")
	}
	via := fmt.Sprint(phone.Eval(`(async () => {
		const stats = await __pcs[0].pc.getStats();
		let out = "no selected pair";
		stats.forEach((r) => {
			if (r.type !== "transport" || !r.selectedCandidatePairId) return;
			const local = stats.get(stats.get(r.selectedCandidatePairId).localCandidateId);
			// A relay candidate, or a peer-reflexive one learned through
			// it (it keeps the relay's relayProtocol and TURN URL): when
			// coturn and the relay share an address on Docker's bridge,
			// as with CI's gateway, Docker masquerades coturn's packets to
			// the relay (they leave on lo with a bridge source), so the
			// relay answers checks with another address than coturn's.
			// The page's policy is relay either way; a candidate that
			// didn't come through TURN has no relayProtocol.
			const viaTURN = local.candidateType === "relay" || (local.candidateType === "prflx" && local.relayProtocol && (local.url || "").startsWith("turn:"));
			out = (viaTURN ? "relay" : local.candidateType) + " over " + local.relayProtocol + (local.candidateType === "prflx" ? " (seen from " + local.address + ")" : "");
		});
		return out;
	})()`))
	if !strings.HasPrefix(via, "relay over ") {
		t.Fatalf("connected, but not through TURN: %s", via)
	}
	ms, _ := phone.Eval(`__pcs[0].ms`).(float64)
	t.Logf("the phone's live view connected through TURN: %s, %v ms after the page made its connection", via, ms)
	// The page waits at most 3 s for candidates; it mustn't spend them on
	// the TURN server it can't reach once another has given it a relay.
	if ms >= 2500 {
		t.Fatalf("connecting took %v ms: the page waited for the unreachable TURN server", ms)
	}
}

// phoneSeesHostAt is the address the phone's container reaches this
// machine at: BROWSERPIER_TEST_SITE_HOST, or what that name resolves to
// there.
func phoneSeesHostAt(t *testing.T, container string) string {
	t.Helper()
	host := siteHost(t)
	if net.ParseIP(host) != nil {
		return host
	}
	out, err := exec.Command("docker", "exec", container, "getent", "ahostsv4", host).Output()
	if f := strings.Fields(string(out)); err == nil && len(f) > 0 && net.ParseIP(f[0]) != nil {
		return f[0]
	}
	t.Fatalf("can't resolve %s in the browser's container: %v", host, err)
	return ""
}
