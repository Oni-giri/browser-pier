package ui

import (
	"testing"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// The profile page's ads label says what the running browser does: a
// profile that blocks, with no list downloaded when the browser started,
// blocks nothing, which isn't "On".
func TestAdsLabel(t *testing.T) {
	d := func(profileBlocks bool, s sessions.Session, read bool) profilePageData {
		return profilePageData{profileView: profileView{Profile: profiles.Profile{BlockAds: profileBlocks}, Session: s}, Blocked: 42, BlockedRead: read}
	}
	running := sessions.Session{State: sessions.Running}
	blocking := running
	blocking.BlockAds = true
	noList := running
	noList.AdListMissing = true
	for _, c := range []struct {
		d    profilePageData
		want string
	}{
		{d(true, noList, false), "No list yet"},
		{d(true, running, false), "Off this session"},
		{d(true, blocking, true), "42 blocked"},
		{d(true, blocking, false), "On"},
		{d(false, running, false), "Off"},
		{d(true, sessions.Session{}, false), "On"},
	} {
		if got := c.d.AdsLabel(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.d.Session, got, c.want)
		}
	}
}
