# Anti-bot validation

How the browser looks to bot detection, in both modes. Checked for PROJ-15 on 2026-10-30, with Chromium 156 on Debian trixie, from an OVH server (a datacenter IP) with the default exit-location matching on (France: `Europe/Paris`, `fr-FR`). See SPEC.md §10 for the approach.

## Automated check

`TestVisualModeLooksLikeAPerson` (`internal/api/antibot_docker_test.go`, `make test-docker`) opens a detection page (`internal/browsertest/detect.go`) in a real browser container. The page reports what detection scripts read back to the test, so Visual mode is checked without CDP.

- **Visual mode:** the page is opened as a person would, through the address bar with xdotool. The test checks:
  - no `navigator.webdriver` and no automation globals
  - a normal user agent, plugins and `window.chrome`
  - a real window size, and a page that has focus
  - a taskbar: `screen.availHeight` under `screen.height`, and the window filling the rest
  - languages, `Intl` and the time zone that match the exit location
  - from the browser's side, **no CDP client attached to the page** (`Target.getTargets` over a browser-level connection, which attaches to nothing)
- **API mode:** the control. A CDP client that enables `Runtime`, as Playwright and Puppeteer do, shows as attached.

## Public test pages

Visual mode was driven only through the address bar, clicks and screenshots. API mode was driven by Playwright over BrowserPier's CDP endpoint, as an agent would.

| Page | Visual mode | API mode |
|---|---|---|
| [bot.sannysoft.com](https://bot.sannysoft.com/) | All pass except **WebGL renderer: SwiftShader** | Same |
| [BrowserScan bot detection](https://www.browserscan.net/bot-detection) | **Normal** (webdriver, user agent, CDP, navigator) | **Normal**: its CDP check doesn't catch Playwright on Chromium 156 |
| [deviceandbrowserinfo: Are you a bot?](https://deviceandbrowserinfo.com/are_you_a_bot) | **"You are human!"**, every signal false | **"You are a bot!"**: `isAutomatedWithCDP` (main thread and workers) |
| [CreepJS](https://abrahamjuliot.github.io/creepjs/) | 0% headless, 0% stealth, 50% "like headless"; 44% since PROJ-29's taskbar | Same |
| [nowsecure.nl](https://nowsecure.nl/), [scrapingcourse.com Cloudflare challenge](https://www.scrapingcourse.com/cloudflare-challenge) | Cloudflare's interactive checkbox | Same |

**How to read these:**
- **CDP is the big difference between the modes**, as SPEC §10 expects. Pages that look for a CDP client find one in API mode and nothing in Visual mode. Use Visual mode for protected sites.
- **Cloudflare challenged both modes** from this datacenter IP. Clicking through it automatically wasn't attempted: that's what handoffs are for. A person finishes it from the handoff link, and the profile keeps the clearance cookie. From a residential connection (a home server, or a residential proxy), fewer challenges are expected.
- **With location matching off,** the browser ran in UTC with English, and deviceandbrowserinfo flagged that combination as a weak bot signal, even in Visual mode. Keep `BROWSERPIER_GEOIP_URL` on, or set a profile's time zone and language by hand.

**Rechecked after PROJ-26,** which sends every connection through the forwarder in the container (no QUIC, and WebRTC over TCP only): Visual mode is still "You are human!" on deviceandbrowserinfo, and sannysoft is unchanged.

## Signals that remain

None of these is spoofed, by design (SPEC §10). They come from the container:

- **SwiftShader WebGL renderer.** There's no GPU in the container, so WebGL is software-rendered and says so. It's the one red line on sannysoft, and a known server signal. Giving containers the host's GPU would fix it where there is one (PROJ-30).
- **A 0×0 window, often, as a page starts.** In both modes, a page's first inline script often sees `outerWidth` and `outerHeight` as 0, which headless browsers report. It reads normally once the page has loaded. A window manager didn't change it (PROJ-29, below). The likely cause, from reading Chromium's source and not verified: a new document's widget hears its window's bounds from the browser only once it's shown, after the commit, while the renderer may already be running the page's first script (`RenderWidgetHostImpl::SendScreenRects` skips hidden widgets). If so, desktop Chrome does the same. PROJ-65 checks both.
- **No speech voices** (`speechSynthesis.getVoices()` is empty), as on many Linux desktops.

## API mode, specifically

- **Focus:** a page opened with CDP's `Page.navigate` leaves the keyboard focus in the address bar, so `document.hasFocus()` is false until something clicks the page. Visual mode, typing a URL and pressing Enter, gives the page focus as a person's browser does.
- **CDP detection:** `isAutomatedWithCDP` and similar checks see the client (see above).

## A desktop with a taskbar (PROJ-29)

Xvfb ran no window manager or panel, so `screen.availHeight` equalled `screen.height`, one of CreepJS's "like headless" hints. The image now runs JWM (Debian's `jwm`, about 7 MB; an empty package stands in for the terminal emulator it depends on, so the image has none) with a 32-pixel taskbar along the bottom, which reserves its strip of the screen through `_NET_WORKAREA` as a person's panel does. Nothing is spoofed: Chromium reads the work area from the window manager. The browser window sits at 0,0 with no frame and fills the screen above the taskbar, so Visual-mode coordinates are still screen coordinates, and the taskbar shows in screenshots and the live view. The taskbar shows only a clock, in the exit's time zone; there are no menus, key bindings or window buttons, so nothing on the desktop starts a program or kills Chromium, and the window can't be moved or resized from the desktop. Fullscreen covers the taskbar, as elsewhere.

Measured on Chromium 156 with `TestMeasureWindowAtLoad` (`go test -tags 'docker measure' -run TestMeasureWindowAtLoad -v ./images/chromium/`, a 1280×800 screen), 110 loads of the detection page each, before and after:

| | Before (no window manager) | After (JWM and taskbar) |
|---|---|---|
| `screen.availHeight` | 800 in 110 of 110 loads | **768** in 110 of 110 |
| Window once loaded | 1279×799 (a pixel short of the screen) | 1280×768, the work area exactly |
| 0×0 at the first script: Visual, fresh containers | 14 of 20 | 14 of 20 |
| … Visual, new tabs | 20 of 30 | 20 of 30 |
| … Visual, the same tab again | 24 of 30 | 28 of 30 |
| … API mode, `Page.navigate` | 16 of 30 | 18 of 30 |

So the taskbar hint is gone, and the 0×0 at load is unchanged (see Signals that remain). On CreepJS, in Visual mode on this machine, "like headless" went from 50% to 44%: `noTaskbar` is now false. Of what it still counts, SwiftShader is the container's; the rest (`noWebShare`, `noContentIndex`, `noContactsManager`, `noDownlinkMax`: APIs desktop Linux Chromium doesn't have, and the default light theme) aren't about the container. The rate depends on timing: these runs, on 2026-11-12, shared 6 CPUs with other work, and PROJ-15 saw about a third.

## Continue where you left off (PROJ-112)

Chromium runs with `RestoreOnStartup: 1`, so it keeps session-only cookies across a stop and reopens its tabs as it starts. That's a common setting in people's browsers. A page sees nothing new: a restored tab loads as it would after a person reopens their browser, and a tab left on a form's answer isn't posted again (`TestRestoredPostPageIsNotResubmitted`). `TestVisualModeLooksLikeAPerson` and `TestPageToolsStayQuiet` pass unchanged with it.

## Ad and tracker blocking (PROJ-119)

Blocking happens in the container's SOCKS forwarder, by host name, before anything leaves the container: Chromium gets SOCKS code 2 for a listed host, and the request fails as any unreachable host's does (`net::ERR_SOCKS_CONNECTION_FAILED` in the network log). Nothing is injected into pages, no extension is installed, and no CDP is attached, so Visual mode stays clean and the fingerprint is unchanged. What a page can see is what it sees with any DNS-level blocker (Pi-hole, NextDNS, a hosts file), which many people run: its ad and analytics scripts don't load. Sites that check for that ("please disable your ad blocker") may say so; challenge and CAPTCHA providers are on a built-in allowlist, so blocking never removes a challenge's own scripts, and a site that insists can be opened with `block_ads: false` for the session.

`TestVisualModeLooksLikeAPerson` and `TestPageToolsStayQuiet` pass with blocking on (2026-11-15, `localhost/bp-agweb-chromium:proj119`): every real-API Docker test's browser now blocks, with a small list of real ad domains, and both tests check their session does. The detector reports the same as before. **Decision: nothing to accept.**

## Input that moves like a hand (PROJ-113)

Before PROJ-113, Visual mode's pointer jumped (`mousemove X Y click`), pressed and released at once, and typed every key 12 ms apart (about 83 characters a second); act moved once to the element's exact centre and pressed with no hold. Pages that score behaviour read exactly that. Now, at human pace (the default; `pace: "instant"` keeps the old input), both modes follow `internal/motion`, a Go port of agweb's Motion model (MIT, see NOTICE):

- **The pointer** glides from where it is (read from X in Visual mode; where act last left it in API mode, or in from an edge of the window at first) along a bowed cubic Bézier with a wobble of about a pixel, at 60 Hz, on minimum-jerk timing whose speed peaks early, in a Fitts's-law time (0.2–1.15 s); hard throws fall short (or now and then overshoot) and correct.
- **The aim** is scattered around the target's middle (SD a sixth of its size, never past 40% of it): in Visual mode, which sees only a point, 8 px (within about 3 px, so a small checkbox or a dense row isn't missed), while the glide's timing assumes 44 px; the element's own box for act, whose chosen point goes through the same hit test as the centre (and must not be on a control inside the element, such as a row's Delete button), and again just before the press.
- **Buttons and keys** go down after a pause (median 0.16 s) and are held about 116 ms. **Typing** has log-normal gaps (median 0.2 s), longer after spaces and punctuation, shorter when the hands alternate, with a hesitation now and then. **Scrolling** turns the wheel in notches that come faster mid-turn.

What a page's own script records (`browsertest.Detector`'s input recorder, `InputURL`), in `TestVisualInputLooksHuman` and `TestActInputLooksHuman`, 2026-11-15, Chromium 156:

| | Visual mode | act (API mode) |
|---|---|---|
| A 400 px click: moves before the press | 45, up to 11 px off the straight line | 43, up to 18 px off it |
| Button held | 60–200 ms | 60–200 ms |
| Ten clicks at one point | scattered, within 3 px | scattered, all on the button |
| Key-down gaps, 30 characters | mean 0.28 s, CV 0.41 | mean 0.20 s, CV 0.44 |
| `isTrusted` | true for every event | (not checked; CDP's are trusted too) |

None of this is fingerprint spoofing: the browser reports nothing different about itself. `TestVisualModeLooksLikeAPerson` and `TestPageToolsStayQuiet` pass unchanged with it. Not done: typing mistakes and corrections, and a check against a public behaviour-scoring page (to be done by hand).

## Text typed right after a shortcut (PROJ-151)

While a page has the keyboard, Chrome sends it every key and moves the focus to its own UI (the address bar for `ctrl+l`) only once the page has answered that it left the key alone. Keys that arrive before that answer are the page's. easi.run found this race in Google Chrome on Wayland; this measures it on BrowserPier's stack.

**2026-11-17, Chromium 156.0.8078.79** (`localhost/bp-proj151-chromium`, built from origin/main with PROJ-113), 1280×800, on a 6-CPU VM shared with other work, load average 1–2. `TestMeasureTextAfterShortcut` (`go test -tags 'docker measure' -run TestMeasureTextAfterShortcut -v ./images/chromium/`, not part of `make test` or `make test-docker`) sends the three calls an agent makes, `key ctrl+l`, `type <the /arrived URL>` and `key Return`, straight to the control socket, back to back at instant pace (the fastest client there can be), on `browsertest.KeySite`'s `/keys` page, whose `keydown` listener busy-waits `slow` ms. A run is **lost** when `/arrived` never loads. One container at a time, no `-race`, under a memory cap. Runs per shape: 100 where quick; **30** where every run is lost, since a lost run behind a 400 ms handler takes about 25 s to clear (each key the page took costs it 400 ms), and the whole matrix at 100 would have taken several hours here; 20 for other shortcuts and the click, as the ticket asked.

| Shape (instant pace, control socket) | Lost | Notes |
|---|---|---|
| `slow=0`, a focused field | 0 of 100 | |
| `slow=0`, nothing focused (body) | 0 of 100 | |
| `slow=50` | 1 of 100 | the page took one character |
| `slow=100` | **30 of 30** | the page took 12.8 characters on average |
| `slow=200` | **30 of 30** | at least 16 characters (a page Chrome replaced while it was still busy stopped reporting) |
| `slow=400` | **30 of 30** | the whole address and Return |
| A cross-site frame with the focus (clicked into), `slow=0` | 1 of 100 | the frame saw no character (below) |
| The same, `slow=400` | **30 of 30** | 48.8 characters: a busy frame holds the shortcut as a busy page does |
| One key call `["ctrl+l", "a", "b", "c"]`, `slow=0` | 0 of 100 | |
| The same, `slow=200` | **30 of 30** | the page's field got all three letters |
| `alt+d`, `F6`, `ctrl+k`, `ctrl+f` then text, `slow=0` | 0 of 20 each | |
| The same, `slow=200` | **20 of 20 each** | `ctrl+k` and `ctrl+f`, which don't navigate, show the page took about 44 of 47 characters |
| `ctrl+t` then text, `slow=0` and `slow=200` | 0 of 20 each | Chromium handles `ctrl+t` before the page (a reserved shortcut), so it can't lose text |
| A click on the address bar (y = 63) then text, `slow=400` | 0 of 20 | the page saw no key: a click on the browser's UI moves the focus without asking the page |
| **Restarted browser**: the restored `/keys` tab takes 2 s to load, the calls sent as soon as the control socket answers | 0 of 20 | the restored page saw a key in 1 run of 20: the focus is in the address bar while it loads |
| **Fresh browser** (no profile), the calls at once | 0 of 10 | the focus starts in the address bar, over the start page (DuckDuckGo's home page here) |
| Over REST (`/computer`), `slow=0` / `slow=200` | 0 of 30 / **30 of 30** | the orchestrator's round trip hides nothing |
| Over MCP (`press_keys`, `type_text`), `slow=0` / `slow=200` | 0 of 30 / **30 of 30** | the same |

**At human pace** (PROJ-113, the default; 10 runs each, as typing an address takes about 13 s): `slow=0` 0 of 10, `slow=100` **9 of 10**, `slow=200` **10 of 10** (the page took the first one or two characters: holding `ctrl+l` for about 116 ms is not enough), restarted 0 of 5, fresh 0 of 3. Over REST and MCP at human pace (10 each): `slow=0` 0 of 10, `slow=200` **10 of 10** (1.4 characters).

Two runs in all (`slow=50` with a 25 ms gap, and the frame at `slow=0`) didn't arrive although the page saw no character but the shortcut's: the tab went back to an earlier `/keys` address, which the address bar suggested from history as the text was typed. That is the address bar's autocomplete, not this race, and it is counted as lost all the same.

**The smallest gap with no loss** (a pause between the end of the `key` call and the `type` call): `slow=0`, the restarted and the fresh browser need none. `slow=50`: 25 ms lost 1 of 100, 50 ms 0 of 100. `slow=100`: 100 ms lost 1 of 100, **150 ms 0 of 100**. `slow=200`: 250 ms still lost 12 of 30 (and 18 more arrived with the page having taken a key). `slow=400`: 250 ms lost 30 of 30. Chrome waits for the page twice, for the `ctrl` key-down and the `l`, so a shortcut's window is about twice the handler's time from the key press.

**Outcome: loss only behind a slow key handler**; a quick page, a restarted browser whose page is still loading and a fresh browser lost nothing. Phase 2 was built anyway, as the ticket decided: the container waits **150 ms** (`shortcutGap` in `entrypoint/visual.go`) between an agent's `key` call ending on a browser-bound combination and its next keyboard action, and inside a key list. The rule asked for the smallest clean gap for `slow=0` and the restarted browser (none) plus a margin; 150 ms is that margin, chosen as the smallest gap measured clean behind a 100 ms handler. With it (`localhost/bp-proj151-chromium:gap`, 30 runs each): `slow=0` 0 of 30, `slow=100` **0 of 30**, `slow=200` 30 of 30, the key list at `slow=200` 30 of 30; the three calls take about 150 ms longer (0.66 s instead of 0.51 s). **The residual:** a page whose key handler takes more than about 100 ms a key still gets the text, as does any page busy for longer than the gap when the shortcut comes. SPEC §8 says so, and `press_keys` tells agents to look at the screen before typing on such a page.

**A real run** with the gap (2026-11-17): a script on the control socket sent `ctrl+l`, an address and Return back to back 20 times, alternating GitHub's golang/go page and Wikipedia's United States article, each round as soon as the last address showed in the tab, the page still loading: 20 of 20 navigated.

Not built, as the ticket rules out: anything over CDP to learn that the browser handled a key. The X display can't tell either: Chromium's toolbar and page are one X window, so `xdotool getwindowfocus` named the same window (4194306, the tab's title) with the page focused and right after `ctrl+l`. The gap changes no event a page sees: the same keys, the next ones only later, as a person's are.

## Opening an address with a click (PROJ-164)

PROJ-151 left a residual: behind a page whose key handler takes more than about 100 ms a key, text typed after `ctrl+l` still goes to the page. Its measurement also found that a click on the address bar moves the focus without asking the page. Visual mode now has an `open {url}` action (MCP `open_url`): a click in the middle of the address bar, `ctrl+a`, the address typed, Delete, Return, all through xdotool at the action's pace (SPEC §8). Before clicking, the container reads the X window tree (`entrypoint/xwin.go`, read-only, no CDP) and refuses if anything but Chromium's tabbed window shows at that point.

**2026-11-18, Chromium 156.0.8078.79** (`localhost/bp-proj164-chromium:open`), on the same shared 6-CPU VM, through the orchestrator's `/computer` (`TestOpenBehindABusyPage`, `TestOpenWithSomethingInTheWay`, `TestOpenAtEveryScreenSize` in `internal/api`, in `make test-docker`):

| Shape | Lost | Notes |
|---|---|---|
| `open`, instant pace, `slow=400`, a focused field | **0 of 20** | the page saw no key and its field stayed empty; about 0.9 s a call for a 60-character address |
| `open`, human pace, `slow=400` | **0 of 3** | the same; 15–16 s a call with the address typed at a typist's pace (before the review; now it goes in at 12 ms a key, see below) |
| `ctrl+l`, `type`, Return through `/computer`, instant pace, `slow=400` (the same test with the old calls) | **lost at once** | the page took 17 keys and its field had `http://host.d` |
| `open` at 640×480 and 1920×1080, `slow=400`, each pace | 0 of 2 each | the address bar is where the entrypoint computes it |
| A page's `alert()` open | opens | the dialog is drawn inside the window, below the toolbar; opening dismisses it |
| A pop-up window the size of the browser window, over it | **refused** (409) | geometry can't tell it from the browser window; its `WM_WINDOW_ROLE` (`pop-up`, not `browser`) does |
| Full screen (F11), with its "press F11 to exit" bubble over the address bar and after it has gone | **refused** (409) | the window fills the whole screen, taskbar included |
| A context menu over the address bar (by hand, with the X tree reader) | refused | an override-redirect window |

**What a page can see.** Nothing of the address: the keys go to the browser's own address bar, never to the page. Before that, at human pace, the pointer glides from wherever it was to the toolbar, so the page sees it move and leave the viewport, then loses the focus (`blur`), as when a person reaches for the address bar; at instant pace it sees no move, only the blur. The new page loads with the focus (`document.hasFocus()` true). `TestVisualModeLooksLikeAPerson` now opens its detector page both ways, with `open` at human pace and with `ctrl+l`: nothing automated either way, and the page focused both times. One difference showed in that run: opened with `open`, the page's first script saw its real window size (1440×868); opened with `ctrl+l` it saw 0×0, the signal PROJ-65 is about. One run each, so this isn't a finding yet.

**After the code review** (same day): the address now goes in at 12 ms a key at either pace (only the glide, the pause and the held button and keys are a hand's), since no page sees those keys and a person often pastes an address; a human-paced open takes about 2 s instead of 15. The last look comes after the hand's pause, just before the press. After the click, which window has the keyboard is read before every 64 characters and before Delete and Return, and the open stops if it changed. After the Codex review (2026-11-26) the look before each of Delete and Return comes after the pause before that key, not ahead of it, so a window that takes the keyboard during a pause gets neither key. It refuses on a Chromium other than 156, whose address bar hasn't been measured. Checked by hand with the window-tree reader: a tab's hover card over the address bar (the pointer resting on a tab) and the address bar's own suggestions are drawn inside the browser window, not as windows of their own; neither stops `open`, and an instant open through a hover card reached the address bar.

**Residuals, accepted:**
- **Delete before Return.** It drops an inline completion the address bar may have added after the typed text (the cause of the two autocomplete losses in PROJ-151's table). A person seldom presses it, but the key goes to the address bar, which no page sees.
- **The address goes in at 12 ms a key**, faster than anyone types, at both paces. No page sees those keys, only the address bar; the search engine's suggest service may see what is typed (PROJ-191).
- **The layout is Chromium 156's.** The address bar's place is computed (y = 63, the middle of x = 121 to width − 85), not found on the screen. On another Chromium `open` refuses (409) rather than click blind, and `TestOpenAtEveryScreenSize` fails until it is measured again.
- **A window that opens between a look and the run after it.** Each look at the screen and the xdotool run it allows are separate: a pop-up opening in those few milliseconds would still take the press (the click, Delete or Return) or up to 64 characters of the address; the next look stops the rest. The pauses all come before a look, never between a look and its press, so this window stays a few milliseconds.

## Fixed along the way

- **`Intl` contradicted the browser's languages.** The image had only Chromium's `en-US` interface, so whatever `LANGUAGE` said, `Intl` reported `en-US` while `navigator.languages` and Accept-Language followed the exit location (`fr-FR`). The image now installs `chromium-l10n` (about 120 MB), and its build fails if the locales are missing. `Intl` now follows the interface language: `de` for a German exit, as Chromium on a German Linux desktop reports.

## API-mode page tools (PROJ-21)

BrowserPier's own page tools (`snapshot`, `find`, `act`, `wait_for`, `navigate`, `tabs`) drive API-mode browsers over CDP from the orchestrator. They are built to stay quieter than Playwright: one short-lived browser-level connection per call through the control socket, **no domain is ever enabled**, scripts run only in an isolated world, and nothing is written into the page. The allow-list is in `internal/pagetools`; the CDP helper that enforces it is `internal/cdp`.

### Phase 0: what works without any `*.enable` (2026-11-12, Chromium 156.0.8078.79)

Measured by `TestSpike` (`internal/cdp/spike_docker_test.go`, run with `BROWSERPIER_SPIKE=1`) through the control socket, sending only the tools' allow-list.

| Question | Result |
|---|---|
| `Accessibility.getFullAXTree`, `DOMSnapshot.captureSnapshot`, `DOM.describeNode`, `DOM.resolveNode` (into an isolated world), `DOM.getContentQuads`, `DOM.scrollIntoViewIfNeeded`, `DOM.focus`, `Page.createIsolatedWorld`, `Runtime.callFunctionOn` / `releaseObject` on isolated objects | All work with nothing enabled, in a few milliseconds on a small page. `Accessibility.getPartialAXTree` (one node, no relatives) works too, and the tools use it to echo what a step hit. So does `DOM.setFileInputFiles` by backend node (PROJ-117, `TestPageToolsUpload`), act's upload step. |
| `Page.handleJavaScriptDialog` | **Doesn't work** without `Page.enable` ("No dialog is showing", even with an `alert()` open). So the tools have **no `dialog` step**. |
| A source for an open dialog's type and message, without `*.enable` | **None.** `Target.getTargets` and `/json/list` don't mention it. While an `alert()` is open, every renderer-side method (`Page.getFrameTree`, `Runtime.evaluate`, `Page.createIsolatedWorld`, the AX tree, the DOM snapshot) gets no answer; browser-side ones (`Page.getNavigationHistory`, `Target.getTargets`) still answer. The tools use that difference to say "the page is busy or has a dialog open; if it stays that way, request a handoff." |
| Keys after `Page.navigate` (focus in the address bar) | `DOM.focus` moves the page's focus and `document.hasFocus()` turns true. Right after a navigation, Chromium drops events until the first raw key-down, so a CDP `keyDown` with text can lose its first character ("abc" arrives as "bc"). Sent as a keyboard sends them, `rawKeyDown`, `char`, `keyUp`, the text always arrives in full; the page can still miss the very first `keydown` event. The tools type that way. |
| `Input.insertText` right after `Page.navigate` | Reaches the focused field in full (7/7 characters into a password field). `fill`'s replace, the `selectAll` editing command then `insertText`, works too. |
| The foreground tab, 20 tabs | `document.visibilityState` (read in an isolated world) is `visible` in exactly one tab: the one created last, or the one `Target.activateTarget` named. Reading all 20 takes 30–80 ms on one connection. A popup window (`window.open` with features) is `visible` and its opener turns `hidden`; a `target=_blank` link opened from a CDP click left its opener in front. Measured without a window manager, and the same again under PROJ-29's JWM (2026-11-14, `BROWSERPIER_SPIKE_ONLY=tabs`; 60–120 ms to read all 20 on a busier machine). |
| Large pages (AX tree / DOM snapshot) | 4,000 elements: 4.0 MB in 0.3 s / 0.9 MB in 0.1 s. 20,000: 20 MB in 2.0 s / 4.9 MB in 0.4 s. 80,000: 81 MB in 28 s / 20 MB in 1.7 s. 200,000: neither answered within 60 s. A real long page (Wikipedia's population list, 10,000 elements): 8.0 MB in 0.5 s / 3.4 MB in 0.3 s. The HTML spec (340,000 elements): no AX tree in 60 s; DOM snapshot 99 MB in 9 s. **The cap:** the tools take the DOM snapshot first, and a page with more than 60,000 DOM nodes (about 30,000 elements) is too large for the AX tree; `snapshot` says so and `find` reads the DOM snapshot's text alone. Messages are capped at 32 MiB, and at most two whole-page reads run at once. |
| Isolated-world build-up | 1,000 calls on one document (each: dial, attach, `getFrameTree`, `createIsolatedWorld`, `evaluate`, close) got the **same** execution context every time (Chromium keeps one world per name and document), with no measurable growth in renderer memory (1,876 → 1,901 MB on an already-large renderer, within noise). About 6 ms of overhead per call. |
| Anything left attached | After the connection closes, `Target.getTargets` shows the page `attached: false`. |
| The front tab, without attaching (PROJ-118, 2026-11-15) | `Target.getTargets` lists tabs in an order of its own, the same whichever tab `Target.activateTarget` brought to front; `/json/list` lists them most recently activated first. A handoff's `until_url` watch, which may not attach while a person has the browser, sends only `Target.getTargets` every 500 ms and so doesn't know the front tab: it counts any tab that has navigated since the handoff opened (SPEC §7). |

### The continuous detector

`browsertest.Watch` (`internal/browsertest/watch.go`) is a page that keeps checking, every 200 ms on its main thread and in a worker, for what pages use to spot CDP automation: the `Error.stack` console leak of an enabled `Runtime`, a logged `Proxy` being inspected, console calls getting slow, `navigator.webdriver` and automation globals; hooks on the DOM methods automation calls in the page's own world (`querySelector`, `getBoundingClientRect`, `focus`, `elementFromPoint`…); and input tells (screen position equal to client position, a click with no mouse movement, a key event without a code, text with no key pressed); and, since PROJ-117, file inputs (an untrusted `input` or `change` event, and files set with no click on the input or a label in the minute before, `file-without-click`). It is shared with PROJ-57.

Console calls getting slow (`console-slow`: 50 `console.debug` calls costing five times what they did at load, and over 50 ms) is timing, so a starved machine trips it as well: on CI runners and on a workstation with its CPUs busy, the page's checks took 5–30 ms now and then, with or without calls. The tests run each batch through `Watch.Quiet`, which, when a batch's only hits are `console-slow`, runs a control batch of as many sleeps with no calls on the same page, and runs the batch again if the control is quiet; slow in the control too, with nothing attached, is the machine's. Measured on Chromium 156, `Runtime.enable` held through 20 calls doesn't make the checks slow at all, so the 50 ms floor (5 ms before 2026-11-14) costs no detection that the positive control shows.

**Positive control** (`browsertest.WatchPositiveControl`: what Playwright does when it connects and acts): detected, through main-world calls (`querySelector`, `getBoundingClientRect`, `focus`), its binding (`__playwright__binding__`), a click with no movement and text with no key pressed. `Runtime.enable` **alone** is no longer detected by the console checks on Chromium 156 (the `Error.stack` leak is closed; getters on an Error's `message` or `name` can't be used, as Chromium's console reads them with nothing attached).

**The tools' CDP calls, 20 runs each, nothing else attached:** nothing recorded, for reading (frame tree, AX tree, partial AX tree, DOM snapshot, isolated world), clicking (scroll into view, quads, hit-test in an isolated world, mouse move, press and release), `fill` (focus, `selectAll`, `insertText`), `type` (focus, key events) and tabs (targets, visibility of each).

**Accepted residuals** (each is what Playwright does as well, and API mode is the mode that isn't for protected sites):
- `fill` puts its text in with one `insertText` after a select-all key press: a detector that checks that typed text matches the keys pressed can tell. Typing with `type` sends one key per character.
- Key events come as fast as CDP sends them, with no typing rhythm.

**The tools themselves, 20 runs each** (`TestPageToolsStayQuiet`, `internal/api/page_tabs_docker_test.go`, in `make test-docker`), nothing else attached: nothing recorded for `snapshot`, `find`, `act` (click, fill, type, press and hover, each its own run), `wait_for` (text, a ref, and `load`, which reads the page's Resource Timing entries from the isolated world, PROJ-122), act and `wait_for` with a role target (PROJ-123: hover, click and wait, each its own run; resolving one is the snapshot's read of the AX tree and DOM snapshot, nothing in the page), `tabs` and `navigate` (reload), while the positive control in the same test was caught (main-world calls and its binding). So there is no hit to decide on; the residuals above still stand.

**act's upload step** (PROJ-117, 2026-11-15, Chromium 156): `DOM.setFileInputFiles` on the input's backend node, with `/files/uploads/<name>` paths. Chromium sets the files and fires `input` then `change` itself, both trusted (`isTrusted`), as when a person picks the files in the chooser; no chooser opens and no script of BrowserPier's touches the page's world. 20 runs on the detector's file input recorded one hit, `file-without-click@main` (the first run's `change`; setting the same file again fires nothing), and no `untrusted-event` or main-world call. **Decision: accepted** and allowed for that batch in `TestPageToolsStayQuiet`. A person opens the chooser with a click on the input or its label, so a page can see files arrive with no click before them; but a person who drops files on the input sends none either, Playwright's `setInputFiles` does the same, and API mode isn't the mode for protected sites. Clicking first to make it look right isn't possible: the click would open the chooser, a window of the browser's own that nothing over CDP can close.

**act's re-centre on a covered element** (PROJ-115): only when a pointer step's hit test finds the element covered (or off screen), act calls the element's `scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'})` once, as a function on its isolated-world object, then keeps hit-testing in the isolated world for up to `cover_wait_ms`. Nothing runs in the page's world; what the page can see is a scroll with no wheel or key event before it, as with `DOM.scrollIntoViewIfNeeded` which every pointer step already sends. The waiting adds no input and nothing the page can see. `TestPageToolsStayQuiet`'s clicks aren't covered, so it never takes this path. **Decision: accepted**, like the scroll into view.

**The Cloudflare challenge look** (PROJ-120, for PROJ-25): the page-info read that `snapshot`, `find`, `navigate` and `page_content` already make in the isolated world also runs a `querySelector` for a mark of Cloudflare's challenge page (`#challenge-form`, `#challenge-stage`, a challenge-platform script or iframe, each one's `src` parsed with `URL` to keep only the page's own origin) and reads `document.title`, then, only on a page with one, `querySelectorAll('input[id^="cf-chl-widget-"]')` and each input's `value`, and, only when that finds the challenge widget's input, its parent's `getBoundingClientRect()` and the window's `devicePixelRatio`, `outerWidth`/`outerHeight` and `screen` size. All of it is in the isolated world, on the browser's own functions: the page's hooks on `querySelector` or `getBoundingClientRect` are in its own world and never run; a forced layout is all a page could notice, as for any read of a box. `TestPageToolsStayQuiet` (2026-11-15) still records nothing for these tools; its detector page has no widget, so the box read isn't in it. The checkbox itself is clicked, if at all, after the agent has checked a screenshot, through xdotool by the agent's `/computer` call or by a person, never over CDP. **Decision: nothing to accept.**

**act's changes** (PROJ-114): after a snapshot, act reads the page again after its steps, to answer with what changed. The read is `snapshot`'s (frame tree, isolated world, DOM snapshot, AX tree) plus one more `Page.getFrameTree` to check the document didn't change during it; nothing new reaches the page, and the diff itself is worked out in the orchestrator. `TestPageToolsStayQuiet` takes a snapshot before its act runs, so every act batch there includes the read, and has a batch with `changes: true` and one of `snapshot` with `diff: true`. **Decision: nothing to accept**, the reads were already measured quiet.

## Page content (PROJ-20)

`POST /api/sessions/{id}/content` (MCP `page_content`) reads the active tab as Markdown or its main text, takes a screenshot or prints a PDF. Its CDP calls go through the same per-call connection as the page tools, with their own allow-list (`contentAllowList` in `internal/pagetools`), and nothing is enabled.

**Measured** on Chromium 156.0.8078.79, 2026-11-13, with a page listening for `beforeprint`, `afterprint`, the print media query, `resize` and `visibilitychange`:

| Call | What the page saw |
|---|---|
| `DOMSnapshot.captureSnapshot` (Markdown and main text) | Nothing. The snapshot walks the flat tree, so shadow roots and slotted content come in place. |
| `Page.printToPDF` | `beforeprint`, the print media query matching, `afterprint`, and the query no longer matching, on every call; no resize. It works in the headful browser (about 25 ms for a small page, under a second for a news front page). |
| `Page.captureScreenshot` | Nothing (viewport only). Not used: screenshots come from the X display, with no CDP at all, in both modes. A full-page capture (`captureBeyondViewport`) resizes the page, which it can see. |

**The content calls, 20 runs each** (`TestContentStaysQuiet`, `internal/api/content_docker_test.go`, in `make test-docker`), nothing else attached, against the continuous detector, which now also reports printing (`print`): nothing recorded for `markdown`, `readability` and `screenshot`; `pdf` recorded `print` and nothing else. The positive control in the same test was caught.

**Decision:** a PDF is a print, and the page sees what it sees when a person presses ctrl+P. Printing is something people do, so it isn't a sign of automation by itself; a page could notice a print with no key pressed before it. Accepted: an agent on a site that watches for it reads the page as Markdown instead, and Visual mode never prints.
