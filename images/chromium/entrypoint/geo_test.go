package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupGeo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, `{"ip":"203.0.113.7","country_iso":"de","time_zone":"Europe/Berlin"}`)
		case "/bad-tz":
			_, _ = io.WriteString(w, `{"ip":"203.0.113.7","country_iso":"DE","time_zone":"Mars/Olympus"}`)
		default:
			http.Error(w, "no", http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	ip, country, tz, err := lookupGeo(context.Background(), srv.Client(), srv.URL+"/ok")
	if err != nil || ip != "203.0.113.7" || country != "DE" || tz != "Europe/Berlin" {
		t.Fatalf("lookupGeo = %s %s %s %v", ip, country, tz, err)
	}
	for _, path := range []string{"/bad-tz", "/limited"} {
		if _, _, _, err := lookupGeo(context.Background(), srv.Client(), srv.URL+path); err == nil {
			t.Errorf("%s: want an error", path)
		}
	}
}

func TestDecideGeo(t *testing.T) {
	found := func(country, tz string) func() (string, string, string, error) {
		return func() (string, string, string, error) { return "203.0.113.7", country, tz, nil }
	}
	failed := func() (string, string, string, error) { return "", "", "", errors.New("timeout") }
	on := config{GeoURL: "https://ifconfig.co/json"}

	g, err := decideGeo(on, true, found("FR", "Europe/Paris"))
	if err != nil || g.Source != "lookup" || g.TimeZone != "Europe/Paris" || g.Locale != "fr-FR" || strings.Join(g.Accept, ",") != "fr-FR,fr,en-US,en" {
		t.Fatalf("fresh lookup: %+v, %v", g, err)
	}

	// Same country, different zone (a rotating US proxy): keep the stored zone.
	stored := on
	stored.StoredCountry, stored.StoredTZ = "US", "America/Chicago"
	if g, _ := decideGeo(stored, true, found("US", "America/New_York")); g.TimeZone != "America/Chicago" || g.Locale != "en-US" {
		t.Fatalf("same country: %+v", g)
	}
	// A new country takes the new zone.
	if g, _ := decideGeo(stored, true, found("DE", "Europe/Berlin")); g.TimeZone != "Europe/Berlin" || g.Country != "DE" {
		t.Fatalf("new country: %+v", g)
	}
	// Lookup failed: the stored value, with a warning.
	if g, err := decideGeo(stored, true, failed); err != nil || g.Source != "stored" || g.TimeZone != "America/Chicago" || g.Warning == "" {
		t.Fatalf("stored fallback: %+v, %v", g, err)
	}
	// Proxied, nothing stored, lookup failed: refuse to start.
	if _, err := decideGeo(on, true, failed); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("proxied with nothing to go on: %v", err)
	}
	// No proxy: start anyway, with a warning.
	if g, err := decideGeo(on, false, failed); err != nil || g.Source != "none" || g.Warning == "" {
		t.Fatalf("direct with nothing: %+v, %v", g, err)
	}
	// Manual settings win, without asking anyone.
	manual := stored
	manual.OverrideTZ, manual.OverrideLocale = "Asia/Tokyo", "ja-JP"
	called := false
	g, _ = decideGeo(manual, true, func() (string, string, string, error) { called = true; return "", "", "", nil })
	if called || g.Source != "override" || g.TimeZone != "Asia/Tokyo" || strings.Join(g.Accept, ",") != "ja-JP,ja,en-US,en" {
		t.Fatalf("override: %+v (lookup called: %t)", g, called)
	}
}

func TestLocaleFor(t *testing.T) {
	for country, want := range map[string]string{"DE": "de-DE", "AT": "de-AT", "BR": "pt-BR", "GB": "en-GB", "TW": "zh-TW", "": "en-US", "ZZ": "en-US"} {
		if got, _ := localeFor(country); got != want {
			t.Errorf("localeFor(%q) = %s, want %s", country, got, want)
		}
	}
	if _, accept := localeFor("GB"); strings.Join(accept, ",") != "en-GB,en" {
		t.Errorf("GB accepts %v", accept)
	}
	env := strings.Join(geo{TimeZone: "Europe/Berlin", Locale: "de-DE"}.env(), " ")
	if env != "TZ=Europe/Berlin LANGUAGE=de_DE:de LANG=de_DE.UTF-8" {
		t.Errorf("env %q", env)
	}
}

func TestApplyAcceptLanguages(t *testing.T) {
	dir := t.TempDir()
	prefsPath := filepath.Join(dir, "Default", "Preferences")
	if err := os.MkdirAll(filepath.Dir(prefsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefsPath, []byte(`{"intl":{"accept_languages":"en-US,en","other":1},"profile":{"name":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		b, _ := os.ReadFile(prefsPath)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	de := geo{Locale: "de-DE", Accept: []string{"de-DE", "de", "en-US", "en"}}
	if err := applyAcceptLanguages(dir, de); err != nil {
		t.Fatal(err)
	}
	m := read()
	intl := m["intl"].(map[string]any)
	if intl["accept_languages"] != "de-DE,de,en-US,en" || intl["other"] != float64(1) || m["profile"] == nil {
		t.Fatalf("prefs after: %v", m)
	}
	// Same locale next time: the person's own changes are left alone.
	intl["accept_languages"] = "de-DE,fr"
	b, _ := json.Marshal(m)
	_ = os.WriteFile(prefsPath, b, 0o600)
	if err := applyAcceptLanguages(dir, de); err != nil {
		t.Fatal(err)
	}
	if read()["intl"].(map[string]any)["accept_languages"] != "de-DE,fr" {
		t.Fatal("rewrote the languages though the locale didn't change")
	}
	// A fresh profile has no Preferences yet.
	fresh := t.TempDir()
	if err := applyAcceptLanguages(fresh, de); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "Default", "Preferences")); err != nil {
		t.Fatal(err)
	}
}
