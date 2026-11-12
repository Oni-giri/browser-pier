package webrtc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // the TURN REST API's HMAC is SHA-1, as coturn checks it
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"time"
)

// TURN says which TURN servers live viewers may relay through, for
// networks that block UDP or direct connections. The orchestrator itself
// doesn't relay: viewers' relayed candidates reach its own port.
type TURN struct {
	// URLs are given to viewers as they are, e.g.
	// turn:pier.example.com:3478?transport=udp. None turns TURN off.
	URLs []string
	// Secret, if set, is the TURN REST API's shared secret (coturn's
	// static-auth-secret): each viewer gets its own credentials, derived
	// from it, that expire. The secret itself never leaves this process.
	Secret string
	// Username and Password are the one static login every viewer gets,
	// for TURN services that offer nothing else. Unused with Secret.
	Username, Password string
	// PeerIPs are this server's addresses as the TURN server reaches it,
	// when they differ from the advertised ones: the orchestrator's
	// address on the compose network, for the bundled coturn. They are
	// advertised too.
	PeerIPs []string
}

// Enabled reports whether viewers are offered a TURN server.
func (t TURN) Enabled() bool { return len(t.URLs) > 0 }

// ICEServer is one entry of RTCPeerConnection's iceServers, as the
// browser takes it.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Servers returns the iceServers for one viewer. With a shared secret its
// credentials are its own and stop working at until.
func (t TURN) Servers(until time.Time) []ICEServer {
	if !t.Enabled() {
		return []ICEServer{}
	}
	s := ICEServer{URLs: append([]string(nil), t.URLs...)}
	switch {
	case t.Secret != "":
		var id [8]byte
		_, _ = rand.Read(id[:])
		s.Username, s.Credential = restCredentials(t.Secret, until, hex.EncodeToString(id[:]))
	case t.Username != "":
		s.Username, s.Credential = t.Username, t.Password
	}
	return []ICEServer{s}
}

// restCredentials are TURN REST API credentials (as coturn's
// use-auth-secret checks them): the username is the expiry as a Unix time
// and an id, the password the base64 HMAC-SHA1 of the username under the
// shared secret.
func restCredentials(secret string, until time.Time, id string) (username, password string) {
	username = strconv.FormatInt(until.Unix(), 10) + ":" + id
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
