package pagetools

import (
	"strings"
	"testing"
)

// TestParseTarget: every accepted form of a role or text target, and the
// errors for what isn't one (PROJ-123).
func TestParseTarget(t *testing.T) {
	for _, c := range []struct {
		in   string
		want target
	}{
		{`role=button`, target{role: "button"}},
		{`role=Button`, target{role: "button"}},
		{`role=PopUpButton`, target{role: "popupbutton"}},
		{`role=button[name="Save"]`, target{role: "button", name: "save", hasName: true}},
		{`role=button[name*="Save"]`, target{role: "button", name: "save", hasName: true}},
		{`role=button[name='Save']`, target{role: "button", name: "save", hasName: true}},
		{`role=button[name=Save]`, target{role: "button", name: "save", hasName: true}},
		{`role=button[ name = "Save" ]`, target{role: "button", name: "save", hasName: true}},
		{`role=link[name="a]b"]`, target{role: "link", name: "a]b", hasName: true}},
		{`role=link[name='a]b']`, target{role: "link", name: "a]b", hasName: true}},
		{`role=button[name='Say "hi"']`, target{role: "button", name: `say "hi"`, hasName: true}},
		{`role=button[name="Say \"hi\""]`, target{role: "button", name: `say "hi"`, hasName: true}},
		{`role=button[name="It's"]`, target{role: "button", name: "it's", hasName: true}},
		{`role=button[name="back\\slash"]`, target{role: "button", name: `back\slash`, hasName: true}},
		// A name cut in a snapshot ends with "…": what is before it is
		// still a part of the name.
		{`role=link[name="Read the whole article about…"]`, target{role: "link", name: "read the whole article about", hasName: true}},
		{`role=heading[name="Été  à  Zürich 日本語"]`, target{role: "heading", name: "été à zürich 日本語", hasName: true}},
		{`role=textbox[name=""]`, target{role: "textbox", hasName: true}},
		{`role=button[name="Save"] >> nth=1`, target{role: "button", name: "save", hasName: true, nth: 1, hasNth: true}},
		{`role=button>>nth=-1`, target{role: "button", nth: -1, hasNth: true}},
		{`role=button[name="a >> nth=3"]`, target{role: "button", name: "a >> nth=3", hasName: true}},
		{`text=Save`, target{text: "save"}},
		{`text=  Save   all  `, target{text: "save all"}},
		{`text="Save"`, target{text: "Save", exact: true}},
		{`text='Save'`, target{text: "Save", exact: true}},
		{`text=" Save  all "`, target{text: "Save all", exact: true}},
		{`text=ÉTÉ`, target{text: "été"}},
		{`text=a >> b`, target{text: "a >> b"}},
		{`text=Save >> nth=0`, target{text: "save", nth: 0, hasNth: true}},
		{`text="Save" >> nth=-1`, target{text: "Save", exact: true, nth: -1, hasNth: true}},
		{`text="a >> nth=1"`, target{text: "a >> nth=1", exact: true}},
		{`text=x]y`, target{text: "x]y"}},
	} {
		got, err := parseTarget(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		got.raw, got.base = "", ""
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, c := range []struct{ in, want string }{
		{`role=buton`, `role "buton" isn't a role`},
		{`role=`, `name a role`},
		{`role=button[name="Save"`, `missing ]`},
		{`role=button[name=Save`, `missing ]`},
		{`role=button[name="Save`, `missing the closing "`},
		{`role=button[label="Save"]`, `only name`},
		{`role=button[name"Save"]`, `want [name="Save"]`},
		{`role=button[name="Save"]x`, `left over`},
		{`role=button >> nth=abc`, `nth=abc: want a number`},
		{`text=Save >> nth=abc`, `nth=abc: want a number`},
		{`text=Save >> nth=`, `want a number`},
		{`text=Save >> nth=99999999999`, `want a number`},
		{`text="Save`, `missing the closing "`},
		{`text="Save" junk`, `left over`},
		{`text=`, `say what text`},
		{`text=""`, `say what text`},
		{`text=  >> nth=0`, `say what text`},
		{`#save`, `CSS selectors aren't accepted`},
		{`button.primary`, `CSS selectors aren't accepted`},
		{`//button`, `CSS selectors aren't accepted`},
		{`a1e2`, `is a ref`},
		{`text=` + strings.Repeat("a", maxTarget), `at most 500 characters`},
	} {
		_, err := parseTarget(c.in)
		if !isStatus(err, 400) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error with %q", c.in, err, c.want)
		}
	}
}

// TestPickNth: nth counts from 0, and from the end when negative.
func TestPickNth(t *testing.T) {
	r := &targetRead{matches: []candidate{{backend: 11}, {backend: 12}, {backend: 13}}}
	for _, c := range []struct {
		in   string
		want int64
	}{
		{`role=button >> nth=0`, 11},
		{`role=button >> nth=2`, 13},
		{`role=button >> nth=-1`, 13},
		{`role=button >> nth=-3`, 11},
	} {
		tg, err := parseTarget(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := (&call{}).pick(tg, r); err != nil || got != c.want {
			t.Errorf("%s: %d %v, want %d", c.in, got, err, c.want)
		}
	}
	one := &targetRead{matches: []candidate{{backend: 7}}}
	tg, _ := parseTarget(`text=Save`)
	if got, err := (&call{}).pick(tg, one); err != nil || got != 7 {
		t.Errorf("one match: %d %v", got, err)
	}
}
