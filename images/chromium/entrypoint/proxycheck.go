package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"time"
)

// proxyCheckArg runs the proxy check instead of the browser. The
// orchestrator starts a short-lived container with it, on the browsers'
// network, to test a proxy before a browser uses it (the admin's Test
// button; see internal/docker's TestProxy).
const proxyCheckArg = "proxy-check"

// The check's outcomes besides the forwarder's own kinds (proxy.go).
const (
	checkLookup    = "lookup"     // through the tunnel, the lookup gave no usable answer
	checkLookupOff = "lookup_off" // BROWSERPIER_GEOIP_URL is off: nothing to test with
	checkFailed    = "failed"     // the check itself couldn't run
)

// checkResult is the check's one line of output. It holds no text of the
// proxy's or the endpoint's beyond the validated values a start would use.
type checkResult struct {
	OK       bool     `json:"ok"`
	IP       string   `json:"ip,omitempty"`
	Country  string   `json:"country,omitempty"`
	TimeZone string   `json:"time_zone,omitempty"`
	Locale   string   `json:"locale,omitempty"`
	Accept   []string `json:"accept_languages,omitempty"`
	MS       int64    `json:"ms,omitempty"` // the lookup's time, proxy handshake included
	Reason   string   `json:"reason,omitempty"`
}

// proxyCheck does what a start does to find the exit, and nothing more:
// it reads (and deletes) the proxy file, builds the forwarder for that
// upstream, and looks up the location through fwd.connect, against geoURL,
// with a start's lookup timeout, which bounds the whole check. It logs nothing: the URL, the user name
// and the password never reach the output.
func proxyCheck(ctx context.Context, proxyFile, geoURL string) checkResult {
	upstream, err := readProxyFile(proxyFile)
	if err != nil || upstream == nil {
		// The orchestrator always hands one over: testing the direct
		// exit isn't what the check is for.
		return checkResult{Reason: checkFailed}
	}
	if geoURL == "" {
		return checkResult{Reason: checkLookupOff}
	}
	fwd, err := newForwarder(upstream, egressPolicy{}, slog.New(slog.DiscardHandler))
	if err != nil {
		return checkResult{Reason: checkFailed}
	}
	// The lookup is a start's, with its timeout (lookupTimeout), which
	// bounds the whole check. Whether the tunnel opened tells a slow
	// proxy from a slow endpoint when that timeout passes.
	// It follows a redirect as a start does (lookupRedirect).
	client, tunnel := tunnelTracked(fwd)
	start := time.Now()
	ip, country, tz, err := lookupGeo(ctx, client, geoURL)
	took := time.Since(start)
	if err != nil {
		return checkResult{Reason: checkReason(err, tunnel.Load())}
	}
	// The address is the endpoint's text: only a plain IP goes out, in
	// its canonical form, and anything else is an unusable answer.
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Zone() != "" {
		return checkResult{Reason: checkLookup}
	}
	ip = addr.String()
	locale, accept := localeFor(country)
	return checkResult{OK: true, IP: ip, Country: country, TimeZone: tz, Locale: locale, Accept: accept,
		MS: max(1, took.Milliseconds())}
}

// checkReason is the kind of a lookup's failure: the forwarder's, if the
// proxy failed; a timeout, if the time ran out before the tunnel opened;
// or else the lookup's own (the tunnel opened, but the endpoint gave no
// usable answer in time). Never the error's text.
func checkReason(err error, tunnelOpened bool) string {
	var pe *proxyError
	switch {
	case errors.As(err, &pe):
		return pe.kind
	case !tunnelOpened && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)):
		return proxyTimeout
	}
	return checkLookup
}

// runProxyCheck prints the check's result as one JSON line, the last of
// the container's output, and exits 0 when the proxy works.
func runProxyCheck(out io.Writer) int {
	res := checkResult{Reason: checkFailed}
	if cfg, err := loadConfig(os.LookupEnv); err == nil {
		res = proxyCheck(context.Background(), cfg.ProxyFile, cfg.GeoURL)
	}
	b, err := json.Marshal(res)
	if err != nil {
		b = []byte(`{"ok":false,"reason":"failed"}`)
	}
	_, _ = fmt.Fprintf(out, "%s\n", b)
	if !res.OK {
		return 1
	}
	return 0
}
