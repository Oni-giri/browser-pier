package browsertest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Coturn starts the TURN server as deploy/docker-compose.yml runs it, from
// that file's own image and start-up script, listening and relaying on ip
// and relaying only to peer. It shares the host's network, where the
// compose file gives it one of its own. It returns host:port once coturn
// accepts connections, and fails the test if coturn logged the secret.
func Coturn(t *testing.T, ip, peer, secret string) string {
	t.Helper()
	image, script := composeTURN(t)
	if docker("image", "inspect", image).Run() != nil {
		if out, err := docker("pull", image).CombinedOutput(); err != nil {
			t.Fatalf("pulling %s: %v\n%s", image, err, out)
		}
	}
	port := freePort(t, ip)
	name := fmt.Sprintf("bp-turn-test-%d", time.Now().UnixNano())
	out, err := docker("run", "-d", "--name", name, "--network", "host",
		"-e", "BROWSERPIER_TURN_SECRET="+secret, "-e", "TURN_IP="+ip, "-e", "BROWSERPIER_IP="+peer, "-e", fmt.Sprint("TURN_PORT=", port),
		"--entrypoint", "/bin/sh", image, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("starting coturn: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		logs, _ := docker("logs", name).CombinedOutput()
		if strings.Contains(string(logs), secret) {
			t.Error("coturn logged the shared secret")
		}
		if t.Failed() {
			t.Logf("coturn's log:\n%s", logs)
		}
		_ = docker("rm", "-f", name).Run()
	})
	addr := net.JoinHostPort(ip, fmt.Sprint(port))
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			return addr
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", name).CombinedOutput()
			t.Fatalf("coturn never listened on %s: %v\n%s", addr, err, logs)
		}
	}
}

// docker runs the Docker CLI, here only on test containers and the
// compose file's image.
func docker(args ...string) *exec.Cmd {
	return exec.Command("docker", args...) //nolint:gosec // see above
}

// composeTURN reads the turn service's image and start-up script from the
// compose file, so what's tested is what ships.
func composeTURN(t *testing.T) (image, script string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "docker-compose.yml"))
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
			in, inCommand = false, false // the next service
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

// freePort finds a port free on ip for both UDP and TCP.
func freePort(t *testing.T, ip string) int {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp", net.JoinHostPort(ip, fmt.Sprint(port)))
		_ = ln.Close()
		if err == nil {
			_ = pc.Close()
			return port
		}
	}
	t.Fatal("no port free for both UDP and TCP")
	return 0
}
