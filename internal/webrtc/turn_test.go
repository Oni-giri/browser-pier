package webrtc

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // as in turn.go
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
	pion "github.com/pion/webrtc/v4"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/db"
)

const testSecret = "north-wind-secret-123"

// The TURN REST API's credentials, as openssl computes them:
//
//	printf %s 1700000000:abc | openssl dgst -sha1 -hmac north-wind-secret-123 -binary | base64
func TestRESTCredentials(t *testing.T) {
	user, pass := restCredentials(testSecret, time.Unix(1700000000, 0), "abc")
	if user != "1700000000:abc" || pass != "gZTMhYpuX3Ap8ezfOCH4cNKmjA0=" {
		t.Fatalf("got %q / %q", user, pass)
	}
}

// userID is the part of a TURN REST username coturn counts quotas by.
func userID(t *testing.T, s []ICEServer) string {
	t.Helper()
	_, id, ok := strings.Cut(s[0].Username, ":")
	if !ok || id == "" {
		t.Fatalf("username %q has no id", s[0].Username)
	}
	return id
}

func TestServersGiveEachViewerItsOwnExpiringCredentials(t *testing.T) {
	if s := (TURN{}).Servers(time.Now(), "handoff h1"); s == nil || len(s) != 0 {
		t.Fatalf("no TURN: %#v, want an empty list (JSON [])", s)
	}
	tr := TURN{URLs: []string{"turn:pier.example.com:3478?transport=udp", "turn:pier.example.com:3478?transport=tcp"}, Secret: testSecret}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	a := tr.Servers(until, "handoff h1")
	if len(a) != 1 || len(a[0].URLs) != 2 {
		t.Fatalf("servers: %#v", a)
	}
	// One viewer keeps its id, whenever it asks and whatever the expiry,
	// so the TURN server's per-user quota holds it; others get their own.
	if again := tr.Servers(until.Add(time.Hour), "handoff h1"); userID(t, again) != userID(t, a) {
		t.Fatal("one viewer got two ids")
	}
	if other := tr.Servers(until, "handoff h2"); userID(t, other) == userID(t, a) {
		t.Fatal("two viewers got the same id")
	}
	if other := (TURN{URLs: tr.URLs, Secret: testSecret + "x"}).Servers(until, "handoff h1"); userID(t, other) == userID(t, a) {
		t.Fatal("the id doesn't depend on the secret, so anyone could work out whose it is")
	}
	if strings.Contains(a[0].Username, "h1") || len(userID(t, a)) != 16 {
		t.Fatalf("the id %q says who the viewer is", userID(t, a))
	}
	exp, _, ok := strings.Cut(a[0].Username, ":")
	if !ok || exp != strconv.FormatInt(until.Unix(), 10) {
		t.Fatalf("username %q doesn't start with the expiry %d", a[0].Username, until.Unix())
	}
	mac := hmac.New(sha1.New, []byte(testSecret))
	mac.Write([]byte(a[0].Username))
	if a[0].Credential != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("the credential isn't the username's HMAC under the secret")
	}
	if strings.Contains(a[0].Credential, testSecret) || strings.Contains(a[0].Username, testSecret) {
		t.Fatal("the secret reached the viewer")
	}

	static := TURN{URLs: []string{"turns:turn.example.net:443"}, Username: "pier", Password: "static-pass"}.Servers(until, "handoff h1")
	if static[0].Username != "pier" || static[0].Credential != "static-pass" {
		t.Fatalf("static login: %#v", static)
	}
}

// candidates returns the addresses an SDP's candidates advertise.
func candidates(sdp string) []string {
	var out []string
	for _, line := range strings.Split(sdp, "\n") {
		if f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "a=")); len(f) > 5 && strings.HasPrefix(f[0], "candidate:") {
			out = append(out, f[4])
		}
	}
	return out
}

func viewerOffer(t *testing.T, cfg pion.Configuration) (*pion.PeerConnection, string) {
	t.Helper()
	pc, err := pion.NewPeerConnection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	return pc, pc.LocalDescription().SDP
}

func noSocket(string) string { return filepath.Join(os.TempDir(), "bp-no-such-socket") }

// The bundled TURN server relays to BrowserPier's address on the compose
// network, so that is advertised too, but only when there is TURN.
func TestRelayAdvertisesTheTURNServersWayIn(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	local := hostIP(t) // as the compose network's address is to BrowserPier
	turn := func(peers ...string) TURN {
		return TURN{URLs: []string{"turn:x:3478"}, Secret: testSecret, PeerIPs: peers}
	}
	for _, c := range []struct {
		name string
		o    Options
		want []string
		not  []string
	}{
		{"with TURN", Options{IPs: []string{"127.0.0.1"}, TURN: turn("10.247.74.2", "127.0.0.1")}, []string{"127.0.0.1", "10.247.74.2"}, nil},
		{"as in the compose file", Options{IPs: []string{"203.0.113.9"}, TURN: turn(local)}, []string{"203.0.113.9", local}, nil},
		{"without TURN", Options{IPs: []string{"127.0.0.1"}, TURN: TURN{PeerIPs: []string{"10.247.74.2"}}}, []string{"127.0.0.1"}, []string{"10.247.74.2"}},
		{"without advertised IPs", Options{TURN: turn("10.247.74.2")}, []string{"10.247.74.2"}, nil},
		{"without advertised IPs, a local peer", Options{TURN: turn(local)}, []string{local}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			// One port for every local address, as in production.
			pc, err := net.ListenPacket("udp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			c.o.Port = pc.LocalAddr().(*net.UDPAddr).Port
			_ = pc.Close()
			r, err := New(c.o, noSocket, log)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			_, offer := viewerOffer(t, pion.Configuration{})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			v, answer, err := r.Connect(ctx, "google", offer, func([]byte, func(string)) {}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			got := candidates(answer)
			// Each address once per transport and component (pion writes
			// every candidate for components 1 and 2), however many local
			// addresses map to it and even when it's a local one itself.
			seen := map[string]bool{}
			for _, line := range strings.Split(answer, "\n") {
				f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "a="))
				if len(f) < 6 || !strings.HasPrefix(f[0], "candidate:") {
					continue
				}
				key := strings.Join([]string{f[1], f[2], f[4], f[5]}, " ") // component, transport, address, port
				if seen[key] {
					t.Errorf("advertised twice: %s", key)
				}
				seen[key] = true
			}
			for _, ip := range c.want {
				if !strings.Contains(strings.Join(got, " "), ip) {
					t.Errorf("%s isn't advertised: %v", ip, got)
				}
			}
			for _, ip := range c.not {
				if strings.Contains(strings.Join(got, " "), ip) {
					t.Errorf("%s is advertised: %v", ip, got)
				}
			}
		})
	}
}

// hostIP is one of this machine's own addresses, not loopback: the relay
// listens on each, but not on loopback.
func hostIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsGlobalUnicast() {
			return n.IP.String()
		}
	}
	t.Skip("no IPv4 address but loopback")
	return ""
}

// turnServer runs a TURN server on ip, over UDP and TCP, that checks TURN
// REST API credentials as coturn does.
func turnServer(t *testing.T, ip string) (udpAddr, tcpAddr string) {
	t.Helper()
	pc, err := net.ListenPacket("udp4", ip+":0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", ip+":0")
	if err != nil {
		t.Fatal(err)
	}
	quiet := logging.NewDefaultLoggerFactory()
	quiet.DefaultLogLevel = logging.LogLevelDisabled
	gen := func() turn.RelayAddressGenerator {
		return &turn.RelayAddressGeneratorStatic{RelayAddress: net.ParseIP(ip), Address: ip}
	}
	s, err := turn.NewServer(turn.ServerConfig{
		Realm:             "browserpier",
		AuthHandler:       turn.LongTermTURNRESTAuthHandler(testSecret, quiet.NewLogger("turn")),
		LoggerFactory:     quiet,
		PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: pc, RelayAddressGenerator: gen()}},
		ListenerConfigs:   []turn.ListenerConfig{{Listener: ln, RelayAddressGenerator: gen()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return pc.LocalAddr().String(), ln.Addr().String()
}

// A viewer allowed only relayed candidates, as on a network that blocks
// direct connections, reaches the relay through TURN with the
// credentials BrowserPier gives it, over UDP and over TCP.
func TestRelayOnlyViewerConnectsThroughTURN(t *testing.T) {
	ip := hostIP(t)
	udpAddr, tcpAddr := turnServer(t, ip)
	for _, url := range []string{"turn:" + udpAddr + "?transport=udp", "turn:" + tcpAddr + "?transport=tcp"} {
		t.Run(url, func(t *testing.T) {
			r, err := New(Options{IPs: []string{ip}, TURN: TURN{URLs: []string{url}, Secret: testSecret}}, noSocket, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			if relayOnlyConnects(t, r, ip, r.ICEServers(time.Now().Add(time.Minute), "test viewer")) != nil {
				t.Fatal("no connection through TURN")
			}
		})
	}
	t.Run("expired credentials", func(t *testing.T) {
		r, err := New(Options{IPs: []string{ip}, TURN: TURN{URLs: []string{"turn:" + udpAddr}, Secret: testSecret}}, noSocket, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		if relayOnlyConnects(t, r, ip, r.ICEServers(time.Now().Add(-time.Minute), "test viewer")) == nil {
			t.Fatal("expired credentials still relay")
		}
	})
}

// relayOnlyConnects connects a relay-only viewer with these ICE servers
// and says "take" on the data channel; nil means the relay heard it.
func relayOnlyConnects(t *testing.T, r *Relay, turnIP string, servers []ICEServer) error {
	t.Helper()
	var ice []pion.ICEServer
	for _, s := range servers {
		ice = append(ice, pion.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	viewer, err := pion.NewPeerConnection(pion.Configuration{ICEServers: ice, ICETransportPolicy: pion.ICETransportPolicyRelay})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = viewer.Close() }()
	if _, err := viewer.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	dc, err := viewer.CreateDataChannel("input", nil)
	if err != nil {
		t.Fatal(err)
	}
	dc.OnOpen(func() { _ = dc.SendText(`{"type":"take"}`) })
	offer, _ := viewer.CreateOffer(nil)
	gathered := pion.GatheringCompletePromise(viewer)
	if err := viewer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	for _, c := range candidates(viewer.LocalDescription().SDP) {
		if c != turnIP {
			t.Fatalf("the viewer offers a candidate that isn't the TURN server's relay: %s", c)
		}
	}
	if len(candidates(viewer.LocalDescription().SDP)) == 0 {
		return context.Canceled // no relayed candidate: TURN refused the credentials
	}
	heard := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v, answer, err := r.Connect(ctx, "google", viewer.LocalDescription().SDP, func(msg []byte, _ func(string)) {
		select {
		case heard <- string(msg):
		default:
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := viewer.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-heard:
		if msg != `{"type":"take"}` {
			t.Fatalf("heard %q", msg)
		}
	case <-time.After(15 * time.Second):
		return context.DeadlineExceeded
	}
	pair, err := viewer.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local.Typ != pion.ICECandidateTypeRelay {
		t.Fatalf("connected, but not through TURN: %v %v", pair, err)
	}
	return nil
}

// The credentials are for someone who may watch: the signed-in admin.
func TestICEServersNeedTheAdmin(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.DiscardHandler)
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	web := auth.NewWeb(admin, auth.NewTokens(d), log, auth.WebOptions{CookieSecure: "auto"})
	r, err := New(Options{IPs: []string{"127.0.0.1"}, TURN: TURN{URLs: []string{"turn:pier.example.com:3478"}, Secret: testSecret}}, noSocket, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	mux := http.NewServeMux()
	NewHandler(r, nil, web, log).Register(mux)

	get := func(cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/live/google/ice", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, cookie := range []string{"", "not-a-session"} {
		if rec := get(cookie); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "credential") {
			t.Fatalf("cookie %q: %d %s", cookie, rec.Code, rec.Body)
		}
	}
	servers := func(cookie string) []ICEServer {
		t.Helper()
		rec := get(cookie)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("signed in: %d %v", rec.Code, rec.Header())
		}
		var out struct {
			ICEServers []ICEServer `json:"iceServers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.ICEServers) != 1 || out.ICEServers[0].Credential == "" {
			t.Fatalf("signed in: %s (%v)", rec.Body, err)
		}
		return out.ICEServers
	}
	cookie, _, err := admin.NewSession(ctx, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	first := servers(cookie)
	exp, _, _ := strings.Cut(first[0].Username, ":")
	n, _ := strconv.ParseInt(exp, 10, 64)
	if left := time.Until(time.Unix(n, 0)); left < LiveTURNLifetime-time.Minute || left > LiveTURNLifetime {
		t.Fatalf("the admin's credentials last %v", left)
	}
	// One signed-in browser is one viewer to the TURN server's quota,
	// however often it asks; another browser is another.
	if userID(t, servers(cookie)) != userID(t, first) {
		t.Fatal("asking again gave the same browser a new TURN id")
	}
	other, _, err := admin.NewSession(ctx, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	if userID(t, servers(other)) == userID(t, first) {
		t.Fatal("two signed-in browsers share a TURN id")
	}
}
