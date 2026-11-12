//go:build docker

// coturn as deploy/docker-compose.yml runs it, against the relay. Here it
// shares the host's network, where the compose file puts it on its own:
// BrowserPier's address and its own are this machine's.
package webrtc

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
	pion "github.com/pion/webrtc/v4"
)

// composeTURN reads the turn service's image and start-up script from the
// compose file, so what's tested is what ships.
func composeTURN(t *testing.T) (image, script string) {
	t.Helper()
	b, err := os.ReadFile("../../deploy/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	in, inCommand := false, false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case l == "  turn:":
			in = true
		case in && len(l) > 2 && l[2] != ' ' && l[2] != '#':
			in = false // the next service
		case in && strings.HasPrefix(l, "    image: "):
			image = strings.TrimPrefix(l, "    image: ")
		case in && l == "    command:":
			inCommand = true
		case inCommand && l == "      - |":
		case inCommand && (l == "" || strings.HasPrefix(l, "        ")):
			lines = append(lines, strings.TrimPrefix(l, "        "))
		case inCommand:
			inCommand = false
		}
	}
	if image == "" || len(lines) < 10 {
		t.Fatalf("no turn service in the compose file (image %q, %d lines of script)", image, len(lines))
	}
	// Compose turns $$ into $.
	return image, strings.ReplaceAll(strings.Join(lines, "\n"), "$$", "$")
}

func freePort(t *testing.T, ip string) int {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp4", ip+":0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp4", fmt.Sprintf("%s:%d", ip, port))
		_ = ln.Close()
		if err == nil {
			_ = pc.Close()
			return port
		}
	}
	t.Fatal("no port free for both UDP and TCP")
	return 0
}

// coturn starts the compose file's TURN server on ip, relaying only to
// peer, and returns its address once it hands out allocations.
func coturn(t *testing.T, ip, peer, secret string) string {
	t.Helper()
	if os.Getenv("BROWSERPIER_TEST_IMAGE") == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	image, script := composeTURN(t)
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		if out, err := exec.Command("docker", "pull", image).CombinedOutput(); err != nil {
			t.Fatalf("pulling %s: %v\n%s", image, err, out)
		}
	}
	port := freePort(t, ip)
	name := fmt.Sprintf("bp-turn-test-%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "run", "-d", "--name", name, "--network", "host",
		"-e", "BROWSERPIER_TURN_SECRET="+secret, "-e", "TURN_IP="+ip, "-e", "BROWSERPIER_IP="+peer, "-e", fmt.Sprint("TURN_PORT=", port),
		"--entrypoint", "/bin/sh", image, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("starting coturn: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if strings.Contains(string(logs), secret) {
			t.Error("coturn logged the shared secret")
		}
		if t.Failed() {
			t.Logf("coturn's log:\n%s", logs)
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	addr := fmt.Sprintf("%s:%d", ip, port)
	user, pass := restCredentials(secret, time.Now().Add(time.Minute), "ready")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		c, err := turnClient(t, addr, user, pass)
		if err == nil {
			c.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("coturn never answered: %v", err)
		}
	}
}

// turnClient allocates a relay on the TURN server at addr.
func turnClient(t *testing.T, addr, user, pass string) (*turn.Client, error) {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	quiet := logging.NewDefaultLoggerFactory()
	quiet.DefaultLogLevel = logging.LogLevelDisabled
	c, err := turn.NewClient(&turn.ClientConfig{STUNServerAddr: addr, TURNServerAddr: addr, Conn: conn, Username: user, Password: pass, LoggerFactory: quiet, RTO: 200 * time.Millisecond})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := c.Listen(); err != nil {
		c.Close()
		_ = conn.Close()
		return nil, err
	}
	if _, err := c.Allocate(); err != nil {
		c.Close()
		_ = conn.Close()
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
