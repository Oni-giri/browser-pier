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
	"strings"
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

// turnClient allocates a UDP relay on the TURN server at addr, reached over
// UDP.
func turnClient(t *testing.T, addr, user, pass string) (*turn.Client, error) {
	t.Helper()
	c, err := newTURNClient(t, addr, user, pass, false, nil)
	if err != nil {
		return nil, err
	}
	if _, err := c.Allocate(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// newTURNClient is a TURN client of the server at addr, over TCP or UDP,
// that hasn't allocated yet.
func newTURNClient(t *testing.T, addr, user, pass string, overTCP bool, family *turn.RequestedAddressFamily) (*turn.Client, error) {
	t.Helper()
	var conn net.PacketConn
	if overTCP {
		tc, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conn = turn.NewSTUNConn(tc)
	} else {
		pc, err := net.ListenPacket("udp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		conn = pc
	}
	t.Cleanup(func() { _ = conn.Close() })
	quiet := logging.NewDefaultLoggerFactory()
	quiet.DefaultLogLevel = logging.LogLevelDisabled
	cfg := &turn.ClientConfig{STUNServerAddr: addr, TURNServerAddr: addr, Conn: conn, Username: user, Password: pass, LoggerFactory: quiet, RTO: 200 * time.Millisecond}
	if family != nil {
		cfg.RequestedAddressFamily = *family
	}
	c, err := turn.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Listen(); err != nil {
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
			s := relay.ICEServers(time.Now().Add(time.Minute), "test viewer")[0]
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
	// IPv6: coturn relays from an IPv4 address only, and denies all of
	// IPv6 besides, so neither an IPv6 peer nor an IPv6 relay is had.
	for _, other := range []string{"::1", "fd00::1", "2001:4860:4860::8888"} {
		if err := c.CreatePermission(&net.UDPAddr{IP: net.ParseIP(other), Port: 53}); err == nil {
			t.Errorf("coturn relays to %s", other)
		}
	}
	v6 := turn.RequestedAddressFamilyIPv6
	if c6, err := newTURNClient(t, addr, user, pass, false, &v6); err != nil {
		t.Fatal(err)
	} else {
		if _, err := c6.Allocate(); err == nil {
			t.Error("coturn gave an IPv6 relay")
		}
		c6.Close()
	}

	// Allow lists are by address, not port: what keeps BrowserPier's other
	// ports (its HTTP API on 7437) out of reach is that coturn relays UDP
	// only. A TCP relay (RFC 6062) is refused: 442, Unsupported Transport
	// Protocol.
	ct, err := newTURNClient(t, addr, user, pass, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ct.Close()
	if _, err := ct.AllocateTCP(); err == nil || !strings.Contains(err.Error(), "442") {
		t.Errorf("a TCP relay: %v, want error 442", err)
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

// TestBundledTURNHoldsEachViewerToItsQuota: a viewer that asks for new
// credentials over and over (a handoff page reloaded, its /ice called in a
// loop) still gets only coturn's user-quota of relays, not the server's
// whole total-quota; another viewer is unaffected.
func TestBundledTURNHoldsEachViewerToItsQuota(t *testing.T) {
	ip := hostIP(t)
	key := secret(t)
	addr := coturn(t, ip, ip, key)
	tr := TURN{URLs: []string{"turn:" + addr}, Secret: key}
	const userQuota = 10 // as in the compose file
	for i := range userQuota + 1 {
		// Fresh credentials each time, as each request to /ice gives.
		s := tr.Servers(time.Now().Add(time.Hour+time.Duration(i)*time.Second), "handoff greedy")[0]
		c, err := turnClient(t, addr, s.Username, s.Credential)
		if i < userQuota {
			if err != nil {
				t.Fatalf("relay %d refused: %v", i+1, err)
			}
			defer c.Close()
			continue
		}
		if err == nil {
			c.Close()
			t.Fatalf("one viewer got %d relays", i+1)
		}
		if !strings.Contains(err.Error(), "486") {
			t.Fatalf("relay %d: %v, want 486 Allocation Quota Reached", i+1, err)
		}
	}
	s := tr.Servers(time.Now().Add(time.Hour), "handoff other")[0]
	c, err := turnClient(t, addr, s.Username, s.Credential)
	if err != nil {
		t.Fatalf("another viewer refused: %v", err)
	}
	c.Close()
}
