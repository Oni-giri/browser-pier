package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// geo is where the browser appears to be, and the time zone and language
// it runs with to match. Nothing is spoofed: these are the settings a
// person living there would have.
type geo struct {
	IP       string   `json:"ip,omitempty"`
	Country  string   `json:"country,omitempty"` // ISO 3166-1 alpha-2
	TimeZone string   `json:"time_zone,omitempty"`
	Locale   string   `json:"locale,omitempty"` // BCP 47, e.g. de-DE
	Accept   []string `json:"accept_languages,omitempty"`
	// Source says where TimeZone came from: override, lookup, stored or none.
	Source  string `json:"source"`
	Warning string `json:"warning,omitempty"`
}

var countryRE = regexp.MustCompile(`^[A-Z]{2}$`)

// lookupTimeout bounds one location lookup, at a start and in the proxy
// check alike.
var lookupTimeout = 5 * time.Second

// maxLookupRedirects is how many redirects one lookup follows at most.
const maxLookupRedirects = 3

// lookupRoots replaces the system's CAs for the lookup's TLS; tests only.
var lookupRoots *x509.CertPool

// lookupRedirect is the lookup's redirect rule, at a start and in the
// proxy check alike (lookupClient): the endpoint the admin set sees the
// exit IP, so a redirect is followed only on its host. The host name stays
// the same (in any case); the scheme may go from http to https, never
// back; the port stays the same, except on that upgrade, which goes to
// https's own port, 443; and at most maxLookupRedirects of them. Any other redirect isn't
// followed: its answer, a 3xx, is a lookup with no usable answer.
func lookupRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || len(via) > maxLookupRedirects {
		return http.ErrUseLastResponse
	}
	from, to := via[len(via)-1].URL, req.URL
	if !strings.EqualFold(from.Hostname(), to.Hostname()) {
		return http.ErrUseLastResponse
	}
	switch {
	case from.Scheme == to.Scheme && urlPort(from) == urlPort(to):
	case from.Scheme == "http" && to.Scheme == "https" && urlPort(to) == "443":
	default:
		return http.ErrUseLastResponse
	}
	return nil
}

// urlPort is u's port, its scheme's default if it names none.
func urlPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// lookupGeo asks an echoip-style endpoint (ifconfig.co/json) where the
// request came from.
func lookupGeo(ctx context.Context, client *http.Client, endpoint string) (ip, country, tz string, err error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The code only: the reason phrase is the answer's own words.
		return "", "", "", geoAnswerError(fmt.Sprintf("%s answered %d", endpoint, resp.StatusCode))
	}
	var v struct {
		IP       string `json:"ip"`
		Country  string `json:"country_iso"`
		TimeZone string `json:"time_zone"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v); err != nil {
		return "", "", "", geoAnswerError(endpoint + " gave no JSON")
	}
	v.Country = strings.ToUpper(v.Country)
	if !countryRE.MatchString(v.Country) {
		return "", "", "", geoAnswerError(endpoint + " gave no country")
	}
	if _, err := time.LoadLocation(v.TimeZone); err != nil || v.TimeZone == "" || v.TimeZone == "Local" {
		return "", "", "", geoAnswerError(endpoint + " gave no usable time zone")
	}
	return v.IP, v.Country, v.TimeZone, nil
}

// geoAnswerError is a lookup's answer that can't be used, in
// BrowserPier's words.
type geoAnswerError string

func (e geoAnswerError) Error() string { return string(e) }

// throughProxy is a failed lookup through a proxy in BrowserPier's own
// words (PROJ-205): the proxy's failure (a proxyError), the answer's, or
// a fixed phrase. Any other error would name the proxy: the tunnel's
// connection is to the proxy, so a reset or a cut-off read names its
// address and port. The text goes into the browser's log and a failed
// start's error, which agents are shown.
func throughProxy(err error) error {
	var pe *proxyError
	var ae geoAnswerError
	var ne net.Error
	switch {
	case errors.As(err, &pe):
		return pe
	case errors.As(err, &ae):
		return ae
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return errors.New("the lookup through the proxy took too long")
	}
	return errors.New("the lookup through the proxy failed after the tunnel opened")
}

// decideGeo picks the time zone and language for this start. In order:
// the profile's manual settings (each one replacing its part); a fresh lookup through the proxy (keeping
// the stored time zone while the country is the same, so a rotating proxy
// doesn't flip the zone between sessions of one account); the stored
// value if the lookup fails; and, with no proxy, nothing (the container's
// UTC), with a warning. A proxied profile with nothing to go on doesn't
// start: a mismatch is what anti-bot systems look for.
func decideGeo(c config, proxied bool, lookup func() (ip, country, tz string, err error)) (geo, error) {
	if c.OverrideTZ != "" && c.OverrideLocale != "" {
		// Both set by hand: nothing to look up.
		return geo{Source: "override", Country: c.StoredCountry, TimeZone: c.OverrideTZ,
			Locale: c.OverrideLocale, Accept: acceptFor(c.OverrideLocale)}, nil
	}
	g, err := lookupOrStored(c, proxied, lookup)
	if err != nil {
		return geo{}, err
	}
	// One of the two set by hand replaces just that part.
	if c.OverrideTZ != "" {
		g.TimeZone = c.OverrideTZ
	}
	if c.OverrideLocale != "" {
		g.Locale, g.Accept = c.OverrideLocale, acceptFor(c.OverrideLocale)
	}
	return g, nil
}

func lookupOrStored(c config, proxied bool, lookup func() (ip, country, tz string, err error)) (geo, error) {
	var lookupErr error
	if c.GeoURL != "" {
		ip, country, tz, err := lookup()
		if err == nil {
			if country == c.StoredCountry && c.StoredTZ != "" {
				tz = c.StoredTZ
			}
			locale, accept := localeFor(country)
			return geo{IP: ip, Country: country, TimeZone: tz, Locale: locale, Accept: accept, Source: "lookup"}, nil
		}
		lookupErr = err
		if proxied {
			lookupErr = throughProxy(err)
		}
	} else {
		lookupErr = errors.New("location lookup is off")
	}
	if c.StoredTZ != "" {
		locale, accept := localeFor(c.StoredCountry)
		return geo{Country: c.StoredCountry, TimeZone: c.StoredTZ, Locale: locale, Accept: accept, Source: "stored",
			Warning: "couldn't check the exit location (" + lookupErr.Error() + "); using the last one"}, nil
	}
	if proxied {
		return geo{}, fmt.Errorf("couldn't find where the proxy exits (%w); set the profile's time zone and language by hand, or fix the lookup", lookupErr)
	}
	return geo{Source: "none", Warning: "couldn't check the exit location (" + lookupErr.Error() + "); the browser runs in UTC with English"}, nil
}

// languages maps a country to its main language. Anything missing gets
// English.
var languages = map[string]string{
	"AR": "es", "AT": "de", "AU": "en", "BE": "fr", "BG": "bg", "BR": "pt", "CA": "en", "CH": "de",
	"CL": "es", "CN": "zh", "CO": "es", "CZ": "cs", "DE": "de", "DK": "da", "EE": "et", "EG": "ar",
	"ES": "es", "FI": "fi", "FR": "fr", "GB": "en", "GR": "el", "HK": "zh", "HR": "hr", "HU": "hu",
	"ID": "id", "IE": "en", "IL": "he", "IN": "en", "IT": "it", "JP": "ja", "KR": "ko", "LT": "lt",
	"LU": "fr", "LV": "lv", "MX": "es", "MY": "ms", "NL": "nl", "NO": "nb", "NZ": "en", "PE": "es",
	"PH": "en", "PL": "pl", "PT": "pt", "RO": "ro", "RS": "sr", "RU": "ru", "SA": "ar", "SE": "sv",
	"SG": "en", "SI": "sl", "SK": "sk", "TH": "th", "TR": "tr", "TW": "zh", "UA": "uk", "US": "en",
	"VN": "vi", "AE": "ar", "ZA": "en",
}

// localeFor returns the locale a person in country would have, and the
// Accept-Language list to go with it.
func localeFor(country string) (string, []string) {
	lang, ok := languages[country]
	if !ok || country == "" {
		return "en-US", []string{"en-US", "en"}
	}
	locale := lang + "-" + country
	return locale, acceptFor(locale)
}

// acceptFor is a realistic Accept-Language list for a locale: the locale,
// its language, then English, as browsers set up in that language send.
func acceptFor(locale string) []string {
	lang, _, _ := strings.Cut(locale, "-")
	out := []string{locale}
	if lang != locale {
		out = append(out, lang)
	}
	if lang != "en" {
		out = append(out, "en-US", "en")
	}
	return out
}

// env is the environment Chromium needs for g: TZ for the clock, LANGUAGE
// and LANG for its interface language and navigator.language.
func (g geo) env() []string {
	var env []string
	if g.TimeZone != "" {
		env = append(env, "TZ="+g.TimeZone)
	}
	if g.Locale != "" {
		lang, _, _ := strings.Cut(g.Locale, "-")
		posix := strings.ReplaceAll(g.Locale, "-", "_")
		env = append(env, "LANGUAGE="+posix+":"+lang, "LANG="+posix+".UTF-8")
	}
	return env
}

// localeMarker records which locale was last written into the profile.
const localeMarker = ".browserpier-locale"

// applyAcceptLanguages writes the Accept-Language list into the profile's
// preferences when the locale has changed since the last start. Chrome
// derives it from the interface language only until the preference is
// first written, then keeps it, so a profile whose proxy moves country
// would otherwise keep sending its old languages.
func applyAcceptLanguages(profileDir string, g geo) error {
	if g.Locale == "" {
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(profileDir, localeMarker)); err == nil && strings.TrimSpace(string(b)) == g.Locale { //nolint:gosec // the container's own profile
		return nil
	}
	path := filepath.Join(profileDir, "Default", "Preferences")
	prefs := map[string]any{}
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // as above
		if err := json.Unmarshal(b, &prefs); err != nil {
			return fmt.Errorf("reading Chromium's preferences: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	intl, _ := prefs["intl"].(map[string]any)
	if intl == nil {
		intl = map[string]any{}
	}
	intl["accept_languages"] = strings.Join(g.Accept, ",")
	prefs["intl"] = intl
	b, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".browserpier-tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(profileDir, localeMarker), []byte(g.Locale+"\n"), 0o600)
}
