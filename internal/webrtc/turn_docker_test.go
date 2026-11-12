//go:build docker

// coturn as deploy/docker-compose.yml runs it, against the relay. Here it
// shares the host's network, where the compose file puts it on its own:
// BrowserPier's address and its own are this machine's.
package webrtc

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
	pion "github.com/pion/webrtc/v4"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
)

// coturn starts the compose file's TURN server on ip, relaying only to
// peer.
func coturn(t *testing.T, ip, peer, secret string) string {
	t.Helper()
	if os.Getenv("BROWSERPIER_TEST_IMAGE") == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	return browsertest.Coturn(t, ip, peer, secret)
}

// turnClient allocates a relay on the TURN server at addr.
func turnClient(t *testing.T, addr, user, pass string) (*turn.Client, error) {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	quiet := logging.NewDefaultLoggerFactory()
	quiet.DefaultLogLevel = logging.LogLevelDisabled
	c, err := turn.NewClient(&turn.ClientConfig{STUNServerAddr: addr, TURNServerAddr: addr, Conn: conn, Username: user, Password: pass, LoggerFactory: quiet, RTO: 200 * time.Millisecond})
	if err != nil {
		return nil, err
	}
	if err := c.Listen(); err != nil {
		c.Close()
		return nil, err
	}
	if _, err := c.Allocate(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func secret(t *testing.T) string {
	t.Helper()
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// TestBundledTURNRelaysARealBrowser: a viewer that may only use relayed
// candidates, as on a network that blocks UDP or direct connections,
// gets a real browser's video through the bundled coturn, over UDP and
// over TCP, with the credentials BrowserPier hands it.
func TestBundledTURNRelaysARealBrowser(t *testing.T) {
	ip := hostIP(t)
	key := secret(t)
	addr := coturn(t, ip, ip, key)
	m, name := startBrowser(t)
	relay, err := New(Options{Port: 0, IPs: []string{ip}, TURN: TURN{URLs: []string{"turn:" + addr}, Secret: key}}, m.Layout().ControlSocket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			s := relay.ICEServers(time.Now().Add(time.Minute))[0]
			ice := []pion.ICEServer{{URLs: []string{"turn:" + addr + "?transport=" + transport}, Username: s.Username, Credential: s.Credential}}
			n := watch(t, relay, name, pion.Configuration{ICEServers: ice, ICETransportPolicy: pion.ICETransportPolicyRelay})
			if n < 30 {
				t.Fatalf("only %d RTP packets in 3 s through TURN over %s", n, transport)
			}
			t.Logf("%d RTP packets in 3 s through TURN over %s", n, transport)
		})
	}
}

// TestBundledTURNIsNoOpenRelay: coturn relays to BrowserPier and nowhere
// else, and only for credentials from its secret that haven't expired.
func TestBundledTURNIsNoOpenRelay(t *testing.T) {
	ip := hostIP(t)
	key := secret(t)
	peer := "10.247.74.2" // as in the compose file
	addr := coturn(t, ip, peer, key)

	user, pass := restCredentials(key, time.Now().Add(time.Minute), "viewer")
	c, err := turnClient(t, addr, user, pass)
	if err != nil {
		t.Fatalf("good credentials refused: %v", err)
	}
	defer c.Close()
	if err := c.CreatePermission(&net.UDPAddr{IP: net.ParseIP(peer), Port: 7438}); err != nil {
		t.Fatalf("BrowserPier refused as a peer: %v", err)
	}
	for _, other := range []string{
		"127.0.0.1",       // the TURN server's own host
		ip,                // the host's address
		"10.247.74.3",     // Caddy, next to BrowserPier
		"172.17.0.1",      // Docker's bridge
		"192.168.1.1",     // a LAN router
		"169.254.169.254", // a cloud's metadata
		"8.8.8.8",         // the internet
	} {
		if err := c.CreatePermission(&net.UDPAddr{IP: net.ParseIP(other), Port: 53}); err == nil {
			t.Errorf("coturn relays to %s", other)
		}
	}

	expiredUser, expiredPass := restCredentials(key, time.Now().Add(-time.Minute), "viewer")
	otherUser, otherPass := restCredentials("another-secret-entirely", time.Now().Add(time.Minute), "viewer")
	for what, cred := range map[string][2]string{
		"expired credentials":             {expiredUser, expiredPass},
		"credentials from another secret": {otherUser, otherPass},
		"a made-up login":                 {"admin", "admin"},
	} {
		if c, err := turnClient(t, addr, cred[0], cred[1]); err == nil {
			c.Close()
			t.Errorf("%s allocate a relay", what)
		}
	}
}
