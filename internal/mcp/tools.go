package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/screenshot"
)

// maxScreens bounds the screen-size cache.
const maxScreens = 1024

func (s *Server) addTools(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "list_profiles", Description: "List the browser profiles this token can use, with whether each is running."}, s.listProfiles)
	sdk.AddTool(srv, &sdk.Tool{Name: "start_browser", Description: "Start a profile's browser, or attach to it if it's already running. Returns the session_id the other tools take."}, s.startBrowser)
	sdk.AddTool(srv, &sdk.Tool{Name: "stop_browser", Description: "Stop a browser to free its memory. Cookies and logins are kept for next time."}, s.stopBrowser)
	sdk.AddTool(srv, &sdk.Tool{Name: "screenshot", Description: "See the browser's screen (Visual mode); the strip along the bottom is the desktop's taskbar, not the page. Other tools take coordinates in this screenshot's pixels."}, s.screenshot)
	sdk.AddTool(srv, &sdk.Tool{Name: "click", Description: "Click at a point of the last screenshot (Visual mode; in API mode too, at a point a page tool gives, such as a Cloudflare checkbox's). At human pace (the default) the pointer glides there, as a hand would, and lands within about 3 screen pixels of the point, so aim at the target's middle. If the click started a download, the answer says so: downloaded name (size), or download in progress when it isn't finished within download_wait_ms (3 s by default)."}, s.click)
	sdk.AddTool(srv, &sdk.Tool{Name: "type_text", Description: "Type text into whatever has focus, as a keyboard would (Visual mode). At human pace (the default) that takes about 0.25 s a character, so at most 150 characters per call; split longer text, or use pace instant."}, s.typeText)
	sdk.AddTool(srv, &sdk.Tool{Name: "press_keys", Description: "Press keys or combinations, such as Return, Tab, ctrl+a or ctrl+shift+t (Visual mode). If they started a download, the answer says so, as click's does."}, s.pressKeys)
	sdk.AddTool(srv, &sdk.Tool{Name: "scroll", Description: "Scroll at a point by whole notches: positive down or right, negative up or left (Visual mode)."}, s.scroll)
	sdk.AddTool(srv, &sdk.Tool{Name: "drag", Description: "Drag from one point to another with the mouse button held (Visual mode)."}, s.drag)
	sdk.AddTool(srv, &sdk.Tool{Name: "wait", Description: "Wait for the page, up to 10 seconds."}, s.wait)
	sdk.AddTool(srv, &sdk.Tool{Name: "zoom", Description: "See a region of the last screenshot at the screen's full resolution, to read small text."}, s.zoom)
	sdk.AddTool(srv, &sdk.Tool{Name: "list_files", Description: "List the files the profile's browser downloaded, newest first; a download still under way isn't listed until it has finished. act, click and press_keys already say what they downloaded."}, s.listFiles)
	s.addUploadTools(srv)
	s.addPageTools(srv)
	s.addHandoffTools(srv)
	s.addDocsTool(srv)
}

// text is a result of one text block.
func text(format string, args ...any) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

type noArgs struct{}

func (s *Server) listProfiles(ctx context.Context, req *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, any, error) {
	var out struct {
		Profiles []struct {
			Name        string `json:"name"`
			DefaultMode string `json:"default_mode"`
			Status      string `json:"status"`
			SessionID   string `json:"session_id"`
		} `json:"profiles"`
	}
	if err := s.callJSON(ctx, req.Extra, "GET", "/api/profiles", nil, &out); err != nil {
		return nil, nil, err
	}
	if len(out.Profiles) == 0 {
		return text("No profiles yet. A person creates them in BrowserPier's web UI and logs in once."), nil, nil
	}
	var b strings.Builder
	for _, p := range out.Profiles {
		fmt.Fprintf(&b, "%s: %s, %s mode", p.Name, p.Status, p.DefaultMode)
		if p.SessionID != "" {
			fmt.Fprintf(&b, ", session_id %s", p.SessionID)
		}
		b.WriteByte('\n')
	}
	return text("%s", b.String()), nil, nil
}

type startArgs struct {
	Profile string `json:"profile" jsonschema:"the profile's name"`
	Mode    string `json:"mode,omitempty" jsonschema:"visual (screenshots and clicks) or api (CDP); default: the profile's own"`
	// AllowAds is block_ads: false on the API.
	AllowAds bool `json:"allow_ads,omitempty" jsonschema:"true turns the profile's ad and tracker blocking off for this session, a running one included, when a page needs what it blocks; it can't be turned back on until the browser restarts"`
}

type sessionOut struct {
	ID          string   `json:"id"`
	Profile     string   `json:"profile"`
	Mode        string   `json:"mode"`
	State       string   `json:"state"`
	Attached    bool     `json:"attached"`
	CDPURL      string   `json:"cdp_url"`
	LiveViewURL string   `json:"live_view_url"`
	InUseBy     []string `json:"in_use_by"`
	BlockAds    bool     `json:"block_ads"`
	// AdListMissing: the profile blocks, but no list was there yet.
	AdListMissing bool `json:"ad_list_missing"`
}

func (s *Server) startBrowser(ctx context.Context, req *sdk.CallToolRequest, in startArgs) (*sdk.CallToolResult, any, error) {
	var out sessionOut
	body := map[string]any{"profile": in.Profile, "mode": in.Mode}
	if in.AllowAds {
		body["block_ads"] = false
	}
	if err := s.callJSON(ctx, req.Extra, "POST", "/api/sessions", body, &out); err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	verb := "Started"
	if out.Attached {
		verb = "Attached to"
	}
	fmt.Fprintf(&b, "%s %s's browser. session_id: %s (%s mode).\n", verb, out.Profile, out.ID, out.Mode)
	if out.Mode == "api" {
		fmt.Fprintf(&b, "CDP: %s (send your token as an Authorization: Bearer header; a client that can't set headers connects to the cdp_url from POST /api/sessions/%s/cdp-ticket instead).\n", out.CDPURL, out.ID)
	} else {
		b.WriteString("Take a screenshot to see it.\n")
	}
	if len(out.InUseBy) > 0 {
		fmt.Fprintf(&b, "Also in use by: %s.\n", strings.Join(out.InUseBy, ", "))
	}
	switch {
	case out.BlockAds:
		b.WriteString("Ads and trackers are blocked; if a page misses something it needs, start_browser again with allow_ads.\n")
	case out.AdListMissing:
		b.WriteString("Ad and tracker blocking is on for this profile, but no list has been downloaded yet, so nothing is blocked this session.\n")
	}
	fmt.Fprintf(&b, "A person can watch at %s.", out.LiveViewURL)
	return text("%s", b.String()), nil, nil
}

type sessionArgs struct {
	SessionID string `json:"session_id" jsonschema:"from start_browser"`
}

func (s *Server) stopBrowser(ctx context.Context, req *sdk.CallToolRequest, in sessionArgs) (*sdk.CallToolResult, any, error) {
	if err := s.callJSON(ctx, req.Extra, "DELETE", "/api/sessions/"+url.PathEscape(in.SessionID), nil, nil); err != nil {
		return nil, nil, err
	}
	s.forgetScreen(in.SessionID)
	return text("Stopped. Its cookies and logins are kept."), nil, nil
}

// screen is a session's screen size, and how screenshots shrink it
// (internal/screenshot, which the page tools' Cloudflare note shares).
type screen = screenshot.Screen

// forgetGone drops a session's screen size once the API says it's gone or
// stopped (it ended some other way than stop_browser).
func (s *Server) forgetGone(id string, r apiReply) {
	if r.status == http.StatusNotFound || r.status == http.StatusConflict {
		s.forgetScreen(id)
	}
}

func (s *Server) forgetScreen(id string) {
	s.screensMu.Lock()
	delete(s.screens, id)
	s.screensMu.Unlock()
}

// screenOf is the session's screen size, asked once.
func (s *Server) screenOf(ctx context.Context, extra *sdk.RequestExtra, id string) (screen, error) {
	s.screensMu.Lock()
	sc, ok := s.screens[id]
	s.screensMu.Unlock()
	if ok {
		return sc, nil
	}
	r, err := s.call(ctx, extra, "GET", "/api/sessions/"+url.PathEscape(id)+"/screenshot?width=16", nil)
	if err != nil {
		return screen{}, err
	}
	if !r.ok() {
		s.forgetGone(id, r)
		return screen{}, toolError(r.errorText())
	}
	return s.rememberScreen(id, r)
}

func (s *Server) rememberScreen(id string, r apiReply) (screen, error) {
	w, _ := strconv.Atoi(r.header.Get("X-Screen-Width"))
	h, _ := strconv.Atoi(r.header.Get("X-Screen-Height"))
	if w <= 0 || h <= 0 {
		return screen{}, fmt.Errorf("the browser didn't say its screen size")
	}
	sc := screen{W: w, H: h}
	s.screensMu.Lock()
	if len(s.screens) >= maxScreens {
		clear(s.screens) // sessions that ended unseen; sizes are asked again
	}
	s.screens[id] = sc
	s.screensMu.Unlock()
	return sc, nil
}

// shotResult is a screenshot for the agent, saying what its pixels are.
func shotResult(png []byte, sc screen, note string) *sdk.CallToolResult {
	sw, sh := sc.Size()
	msg := fmt.Sprintf("Screenshot, %d×%d. Give coordinates in these pixels.", sw, sh)
	if note != "" {
		msg = note + " " + msg
	}
	return &sdk.CallToolResult{Content: []sdk.Content{
		&sdk.TextContent{Text: msg},
		&sdk.ImageContent{Data: png, MIMEType: "image/png"},
	}}
}

func (s *Server) screenshot(ctx context.Context, req *sdk.CallToolRequest, in sessionArgs) (*sdk.CallToolResult, any, error) {
	sc, err := s.screenOf(ctx, req.Extra, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	sw, _ := sc.Size()
	r, err := s.call(ctx, req.Extra, "GET", "/api/sessions/"+url.PathEscape(in.SessionID)+"/screenshot?width="+strconv.Itoa(sw), nil)
	if err != nil {
		return nil, nil, err
	}
	if !r.ok() {
		s.forgetGone(in.SessionID, r)
		return nil, nil, toolError(r.errorText())
	}
	if sc, err = s.rememberScreen(in.SessionID, r); err != nil {
		return nil, nil, err
	}
	return shotResult(r.body, sc, ""), nil, nil
}

// act runs a Visual-mode action through the API's /computer, in screen
// pixels, returning a screenshot after it if asked.
func (s *Server) act(ctx context.Context, extra *sdk.RequestExtra, id string, sc screen, action map[string]any, shot bool, done string) (*sdk.CallToolResult, any, error) {
	if p, _ := action["pace"].(string); p == "" {
		delete(action, "pace")
	}
	if w, ok := action["download_wait_ms"].(*int); ok && w == nil {
		delete(action, "download_wait_ms")
	}
	if shot {
		sw, _ := sc.Size()
		action["screenshot"] = true
		action["width"] = sw
	}
	r, err := s.call(ctx, extra, "POST", "/api/sessions/"+url.PathEscape(id)+"/computer", action)
	if err != nil {
		return nil, nil, err
	}
	if !r.ok() {
		s.forgetGone(id, r)
		return nil, nil, toolError(r.errorText())
	}
	if shot && strings.HasPrefix(r.header.Get("Content-Type"), "image/") {
		res := shotResult(r.body, sc, done)
		if n := downloadsText(r.header); n != "" {
			res.Content = append(res.Content, &sdk.TextContent{Text: n})
		}
		return res, nil, nil
	}
	if e := r.header.Get("X-Screenshot-Error"); shot && e != "" {
		done = fmt.Sprintf("%s The screenshot after it failed (%s); take one with screenshot.", done, e)
	}
	if n := downloadsText(r.header); n != "" {
		done += "\n" + n
	}
	return text("%s", done), nil, nil
}

// downloadsText is a click's or key press's download notice (the API's
// X-Downloads header) as text: one line per file, fenced, since the names
// come from the pages that offered the downloads. "" if there is none.
func downloadsText(h http.Header) string {
	raw := h.Get(api.DownloadsHeader)
	if raw == "" {
		return ""
	}
	var n struct {
		Files []files.Download `json:"files"`
		More  int              `json:"more"`
	}
	if json.Unmarshal([]byte(raw), &n) != nil || len(n.Files) == 0 {
		return ""
	}
	lines := files.Lines(n.Files)
	if n.More > 0 {
		lines = append(lines, fmt.Sprintf("and %d more", n.More))
	}
	return "Downloads:\n" + fence.Wrap("downloads", strings.Join(lines, "\n"))
}

type pointArgs struct {
	SessionID  string `json:"session_id" jsonschema:"from start_browser"`
	X          int    `json:"x" jsonschema:"in the last screenshot's pixels"`
	Y          int    `json:"y" jsonschema:"in the last screenshot's pixels"`
	Screenshot bool   `json:"screenshot,omitempty" jsonschema:"return a screenshot after"`
	Pace       string `json:"pace,omitempty" jsonschema:"human (default): moves, aims and types like a hand, so it takes longer; instant: at once"`
}

type clickArgs struct {
	pointArgs
	Button string `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	Double bool   `json:"double,omitempty" jsonschema:"double-click"`
	// DownloadWaitMS is a pointer, so 0 (don't wait) differs from unset.
	DownloadWaitMS *int `json:"download_wait_ms,omitempty" jsonschema:"how long to wait for a download the click started to finish, in ms; default 3000, 0 not to wait, at most 30000; when set, also how long to look for one to start, so a slow server's download is told now rather than with the next action"`
}

func (s *Server) click(ctx context.Context, req *sdk.CallToolRequest, in clickArgs) (*sdk.CallToolResult, any, error) {
	sc, err := s.screenOf(ctx, req.Extra, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	x, y := sc.ToScreen(in.X, in.Y)
	return s.act(ctx, req.Extra, in.SessionID, sc, map[string]any{"action": "click", "x": x, "y": y, "button": in.Button, "double": in.Double, "pace": in.Pace, "download_wait_ms": in.DownloadWaitMS}, in.Screenshot, "Clicked.")
}

type typeArgs struct {
	SessionID  string `json:"session_id" jsonschema:"from start_browser"`
	Text       string `json:"text" jsonschema:"what to type; use press_keys for Return, Tab and shortcuts. At most 150 characters at human pace (about 0.25 s a character), 2000 at instant"`
	Screenshot bool   `json:"screenshot,omitempty" jsonschema:"return a screenshot after"`
	Pace       string `json:"pace,omitempty" jsonschema:"human (default): moves, aims and types like a hand, so it takes longer; instant: at once"`
}

// maxHumanType caps what one MCP call types at human pace. At about a
// quarter of a second a character, 150 characters take about 40 s, which
// ends before an MCP client's request timeout (60 s by default in the
// TypeScript SDK): a client that gave up mid-type would retry, and the
// text would be typed twice. The REST API, whose clients set their own
// timeouts, takes 500.
const maxHumanType = 150

// humanPace reports whether a call's pace is human, the default.
func humanPace(pace string) bool { return pace == "" || pace == "human" }

// tooLongToType refuses human-paced text over maxHumanType characters.
func tooLongToType(what string, n int, fill bool) error {
	if n <= maxHumanType {
		return nil
	}
	alt := "split it across calls, or set pace instant"
	if fill {
		alt = "split them across calls, use fill, or set pace instant"
	}
	return toolError(fmt.Sprintf("%s: %d characters at human pace, at most %d per call (about 0.25 s a character, so the call ends before your client's timeout); %s", what, n, maxHumanType, alt))
}

func (s *Server) typeText(ctx context.Context, req *sdk.CallToolRequest, in typeArgs) (*sdk.CallToolResult, any, error) {
	if humanPace(in.Pace) {
		if err := tooLongToType("text", utf8.RuneCountInString(in.Text), false); err != nil {
			return nil, nil, err
		}
	}
	var sc screen
	if in.Screenshot { // the size only matters for the screenshot after
		var err error
		if sc, err = s.screenOf(ctx, req.Extra, in.SessionID); err != nil {
			return nil, nil, err
		}
	}
	return s.act(ctx, req.Extra, in.SessionID, sc, map[string]any{"action": "type", "text": in.Text, "pace": in.Pace}, in.Screenshot, "Typed.")
}

type keysArgs struct {
	SessionID  string `json:"session_id" jsonschema:"from start_browser"`
	Keys       string `json:"keys" jsonschema:"a key or combination, or several separated by spaces: Return, Tab, Escape, ctrl+a, ctrl+shift+t"`
	Screenshot bool   `json:"screenshot,omitempty" jsonschema:"return a screenshot after"`
	Pace       string `json:"pace,omitempty" jsonschema:"human (default): moves, aims and types like a hand, so it takes longer; instant: at once"`
	// DownloadWaitMS is a pointer, so 0 (don't wait) differs from unset.
	DownloadWaitMS *int `json:"download_wait_ms,omitempty" jsonschema:"how long to wait for a download the keys started to finish, in ms; default 3000, 0 not to wait, at most 30000; when set, also how long to look for one to start, so a slow server's download is told now rather than with the next action"`
}

func (s *Server) pressKeys(ctx context.Context, req *sdk.CallToolRequest, in keysArgs) (*sdk.CallToolResult, any, error) {
	var sc screen
	if in.Screenshot { // the size only matters for the screenshot after
		var err error
		if sc, err = s.screenOf(ctx, req.Extra, in.SessionID); err != nil {
			return nil, nil, err
		}
	}
	return s.act(ctx, req.Extra, in.SessionID, sc, map[string]any{"action": "key", "keys": strings.Fields(in.Keys), "pace": in.Pace, "download_wait_ms": in.DownloadWaitMS}, in.Screenshot, "Pressed "+in.Keys+".")
}

type scrollArgs struct {
	pointArgs
	Down  int `json:"down,omitempty" jsonschema:"notches to scroll down; negative scrolls up"`
	Right int `json:"right,omitempty" jsonschema:"notches to scroll right; negative scrolls left"`
}

func (s *Server) scroll(ctx context.Context, req *sdk.CallToolRequest, in scrollArgs) (*sdk.CallToolResult, any, error) {
	sc, err := s.screenOf(ctx, req.Extra, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	x, y := sc.ToScreen(in.X, in.Y)
	return s.act(ctx, req.Extra, in.SessionID, sc, map[string]any{"action": "scroll", "x": x, "y": y, "dx": in.Right, "dy": in.Down, "pace": in.Pace}, in.Screenshot, "Scrolled.")
}

type dragArgs struct {
	SessionID  string `json:"session_id" jsonschema:"from start_browser"`
	FromX      int    `json:"from_x" jsonschema:"in the last screenshot's pixels"`
	FromY      int    `json:"from_y"`
	ToX        int    `json:"to_x"`
	ToY        int    `json:"to_y"`
	Screenshot bool   `json:"screenshot,omitempty" jsonschema:"return a screenshot after"`
	Pace       string `json:"pace,omitempty" jsonschema:"human (default): moves, aims and types like a hand, so it takes longer; instant: at once"`
}

func (s *Server) drag(ctx context.Context, req *sdk.CallToolRequest, in dragArgs) (*sdk.CallToolResult, any, error) {
	sc, err := s.screenOf(ctx, req.Extra, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	fx, fy := sc.ToScreen(in.FromX, in.FromY)
	tx, ty := sc.ToScreen(in.ToX, in.ToY)
	return s.act(ctx, req.Extra, in.SessionID, sc, map[string]any{"action": "drag",
		"from": map[string]int{"x": fx, "y": fy}, "to": map[string]int{"x": tx, "y": ty}, "pace": in.Pace}, in.Screenshot, "Dragged.")
}

type waitArgs struct {
	SessionID  string  `json:"session_id" jsonschema:"from start_browser"`
	Seconds    float64 `json:"seconds" jsonschema:"up to 10"`
	Screenshot bool    `json:"screenshot,omitempty" jsonschema:"return a screenshot after"`
}

func (s *Server) wait(ctx context.Context, req *sdk.CallToolRequest, in waitArgs) (*sdk.CallToolResult, any, error) {
	var sc screen
	if in.Screenshot { // the size only matters for the screenshot after
		var err error
		if sc, err = s.screenOf(ctx, req.Extra, in.SessionID); err != nil {
			return nil, nil, err
		}
	}
	ms := int(min(max(in.Seconds, 0), 10) * 1000)
	return s.act(ctx, req.Extra, in.SessionID, sc, map[string]any{"action": "wait", "ms": ms}, in.Screenshot,
		fmt.Sprintf("Waited %s.", time.Duration(ms)*time.Millisecond))
}

type zoomArgs struct {
	SessionID string `json:"session_id" jsonschema:"from start_browser"`
	X         int    `json:"x" jsonschema:"the region's left edge, in the last screenshot's pixels"`
	Y         int    `json:"y" jsonschema:"the region's top edge"`
	Width     int    `json:"width" jsonschema:"the region's width"`
	Height    int    `json:"height" jsonschema:"the region's height"`
}

func (s *Server) zoom(ctx context.Context, req *sdk.CallToolRequest, in zoomArgs) (*sdk.CallToolResult, any, error) {
	sc, err := s.screenOf(ctx, req.Extra, in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if in.Width <= 0 || in.Height <= 0 {
		return nil, nil, toolError("give the region a width and a height")
	}
	x, y := sc.ToScreen(in.X, in.Y)
	x2, y2 := sc.Scale(in.X+in.Width, in.Y+in.Height)
	r, err := s.call(ctx, req.Extra, "POST", "/api/sessions/"+url.PathEscape(in.SessionID)+"/computer",
		map[string]any{"action": "zoom", "x": x, "y": y, "w": max(1, x2-x), "h": max(1, y2-y)})
	if err != nil {
		return nil, nil, err
	}
	if !r.ok() {
		return nil, nil, toolError(r.errorText())
	}
	msg := "The region at full resolution. Coordinates for other tools are still in the screenshot's pixels."
	if cfg, err := png.DecodeConfig(bytes.NewReader(r.body)); err == nil {
		msg = fmt.Sprintf("The region at full resolution, %d×%d. Coordinates for other tools are still in the screenshot's pixels.", cfg.Width, cfg.Height)
	}
	return &sdk.CallToolResult{Content: []sdk.Content{
		&sdk.TextContent{Text: msg},
		&sdk.ImageContent{Data: r.body, MIMEType: "image/png"},
	}}, nil, nil
}

type profileArgs struct {
	Profile string `json:"profile" jsonschema:"the profile's name"`
}

func (s *Server) listFiles(ctx context.Context, req *sdk.CallToolRequest, in profileArgs) (*sdk.CallToolResult, any, error) {
	var out struct {
		Files []struct {
			Name     string    `json:"name"`
			Size     int64     `json:"size"`
			Modified time.Time `json:"modified"`
		} `json:"files"`
	}
	if err := s.callJSON(ctx, req.Extra, "GET", "/api/profiles/"+url.PathEscape(in.Profile)+"/files", nil, &out); err != nil {
		return nil, nil, err
	}
	if len(out.Files) == 0 {
		return text("No files."), nil, nil
	}
	// File names come from the pages that offered the downloads.
	var b strings.Builder
	for _, f := range out.Files {
		fmt.Fprintf(&b, "%s (%d bytes, %s)\n", f.Name, f.Size, f.Modified.UTC().Format(time.RFC3339))
	}
	return text("%s\nFetch one with GET /api/profiles/%s/files/{name} and your token.", fence.Wrap("downloaded file names", b.String()), in.Profile), nil, nil
}
