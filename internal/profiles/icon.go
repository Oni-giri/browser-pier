package profiles

import (
	"fmt"
	"hash/fnv"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Glyphs are the fallback icons a profile can show when its site's favicon
// isn't known. The UI draws each as an inline SVG of the same name.
var Glyphs = []string{
	"envelope", "briefcase", "code", "document", "chat", "cart",
	"globe", "bank", "calendar", "cloud", "chart", "key",
}

// Colors are the fallback icon's tints: cool hues only. Orange means a
// person is needed and red means failure, so neither is ever an icon.
var Colors = []string{"blue", "sky", "violet", "indigo", "teal", "cyan", "slate"}

// MaxSite is the longest hostname DNS allows.
const MaxSite = 253

var labelRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Icon is the fallback glyph and colour a profile shows: the ones chosen,
// or for each left empty, one picked from its name, so it stays the same.
func Icon(p Profile) (glyph, color string) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(p.Name))
	sum := h.Sum64()
	glyph, color = p.IconGlyph, p.IconColor
	if glyph == "" {
		glyph = Glyphs[sum%uint64(len(Glyphs))]
	}
	if color == "" {
		// Other bits of the hash, so the colour doesn't follow the glyph.
		color = Colors[(sum>>32)%uint64(len(Colors))]
	}
	return glyph, color
}

// Glyph is the fallback glyph the profile shows (see Icon).
func (p Profile) Glyph() string { g, _ := Icon(p); return g }

// Color is the fallback icon's colour (see Icon).
func (p Profile) Color() string { _, c := Icon(p); return c }

// EffectiveSite is the site the profile's icon comes from: the one set,
// else the one read from its history.
func (p Profile) EffectiveSite() string {
	if p.Site != "" {
		return p.Site
	}
	return p.SiteInferred
}

// NormalizeSite turns what someone typed for a main site, such as
// "https://Mail.Google.com/inbox", into the hostname Validate expects. It
// returns what it can't make sense of as is, for Validate to refuse. An
// internationalised name is taken in its xn-- form only.
func NormalizeSite(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "//" + s
	}
	if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
		s = u.Hostname()
	} else {
		return strings.TrimSpace(raw)
	}
	return strings.TrimSuffix(s, ".")
}

// DropInvalidIcon clears the site and fallback icon fields that wouldn't
// pass Validate, for settings from elsewhere (an archive from a later
// version, say), where the icon isn't worth refusing them for.
func (p *Profile) DropInvalidIcon() {
	if validSite(p.Site) != nil {
		p.Site = ""
	}
	if validSite(p.SiteInferred) != nil {
		p.SiteInferred = ""
	}
	if validIcon(p.IconGlyph, "") != nil {
		p.IconGlyph = ""
	}
	if validIcon("", p.IconColor) != nil {
		p.IconColor = ""
	}
}

func validSite(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > MaxSite {
		return fmt.Errorf("main site: at most %d characters", MaxSite)
	}
	// An IPv4 address passes too, as digits; an IPv6 one doesn't.
	for label := range strings.SplitSeq(s, ".") {
		if !labelRE.MatchString(label) {
			return fmt.Errorf("main site %q: want a hostname such as mail.google.com", s)
		}
	}
	return nil
}

func validIcon(glyph, color string) error {
	if glyph != "" && !slices.Contains(Glyphs, glyph) {
		return fmt.Errorf("icon %q: want one of %s", glyph, strings.Join(Glyphs, ", "))
	}
	if color != "" && !slices.Contains(Colors, color) {
		return fmt.Errorf("icon colour %q: want one of %s", color, strings.Join(Colors, ", "))
	}
	return nil
}
