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

## Input that moves like a hand (PROJ-113)

Before PROJ-113, Visual mode's pointer jumped (`mousemove X Y click`), pressed and released at once, and typed every key 12 ms apart (about 83 characters a second); act moved once to the element's exact centre and pressed with no hold. Pages that score behaviour read exactly that. Now, at human pace (the default; `pace: "instant"` keeps the old input), both modes follow `internal/motion`, a Go port of agweb's Motion model (MIT, see NOTICE):

- **The pointer** glides from where it is (read from X in Visual mode; where act last left it in API mode, or in from an edge of the window at first) along a bowed cubic Bézier with a wobble of about a pixel, at 60 Hz, on minimum-jerk timing whose speed peaks early, in a Fitts's-law time (0.2–1.15 s); hard throws fall short (or now and then overshoot) and correct.
- **The aim** is scattered around the target's middle (SD a sixth of its size, never past 40% of it): 44 px in Visual mode, which sees only a point; the element's own box for act, whose chosen point goes through the same hit test as the centre, and again just before the press.
- **Buttons and keys** go down after a pause (median 0.16 s) and are held about 116 ms. **Typing** has log-normal gaps (median 0.2 s), longer after spaces and punctuation, shorter when the hands alternate, with a hesitation now and then. **Scrolling** turns the wheel in notches that come faster mid-turn.

What a page's own script records (`browsertest.Detector`'s input recorder, `InputURL`), in `TestVisualInputLooksHuman` and `TestActInputLooksHuman`, 2026-11-15, Chromium 156:

| | Visual mode | act (API mode) |
|---|---|---|
| A 400 px click: moves before the press | 45, up to 11 px off the straight line | 43, up to 18 px off it |
| Button held | 60–200 ms | 60–200 ms |
| Ten clicks at one point | scattered, within 18 px | scattered, all on the button |
| Key-down gaps, 30 characters | mean 0.28 s, CV 0.41 | mean 0.20 s, CV 0.44 |
| `isTrusted` | true for every event | (not checked; CDP's are trusted too) |

None of this is fingerprint spoofing: the browser reports nothing different about itself. `TestVisualModeLooksLikeAPerson` and `TestPageToolsStayQuiet` pass unchanged with it. Not done: typing mistakes and corrections, and a check against a public behaviour-scoring page (to be done by hand).

## Fixed along the way

- **`Intl` contradicted the browser's languages.** The image had only Chromium's `en-US` interface, so whatever `LANGUAGE` said, `Intl` reported `en-US` while `navigator.languages` and Accept-Language followed the exit location (`fr-FR`). The image now installs `chromium-l10n` (about 120 MB), and its build fails if the locales are missing. `Intl` now follows the interface language: `de` for a German exit, as Chromium on a German Linux desktop reports.

## API-mode page tools (PROJ-21)

BrowserPier's own page tools (`snapshot`, `find`, `act`, `wait_for`, `navigate`, `tabs`) drive API-mode browsers over CDP from the orchestrator. They are built to stay quieter than Playwright: one short-lived browser-level connection per call through the control socket, **no domain is ever enabled**, scripts run only in an isolated world, and nothing is written into the page. The allow-list is in `internal/pagetools`; the CDP helper that enforces it is `internal/cdp`.

### Phase 0: what works without any `*.enable` (2026-11-12, Chromium 156.0.8078.79)

Measured by `TestSpike` (`internal/cdp/spike_docker_test.go`, run with `BROWSERPIER_SPIKE=1`) through the control socket, sending only the tools' allow-list.

| Question | Result |
|---|---|
| `Accessibility.getFullAXTree`, `DOMSnapshot.captureSnapshot`, `DOM.describeNode`, `DOM.resolveNode` (into an isolated world), `DOM.getContentQuads`, `DOM.scrollIntoViewIfNeeded`, `DOM.focus`, `Page.createIsolatedWorld`, `Runtime.callFunctionOn` / `releaseObject` on isolated objects | All work with nothing enabled, in a few milliseconds on a small page. `Accessibility.getPartialAXTree` (one node, no relatives) works too, and the tools use it to echo what a step hit. |
| `Page.handleJavaScriptDialog` | **Doesn't work** without `Page.enable` ("No dialog is showing", even with an `alert()` open). So the tools have **no `dialog` step**. |
| A source for an open dialog's type and message, without `*.enable` | **None.** `Target.getTargets` and `/json/list` don't mention it. While an `alert()` is open, every renderer-side method (`Page.getFrameTree`, `Runtime.evaluate`, `Page.createIsolatedWorld`, the AX tree, the DOM snapshot) gets no answer; browser-side ones (`Page.getNavigationHistory`, `Target.getTargets`) still answer. The tools use that difference to say "the page is busy or has a dialog open; if it stays that way, request a handoff." |
| Keys after `Page.navigate` (focus in the address bar) | `DOM.focus` moves the page's focus and `document.hasFocus()` turns true. Right after a navigation, Chromium drops events until the first raw key-down, so a CDP `keyDown` with text can lose its first character ("abc" arrives as "bc"). Sent as a keyboard sends them, `rawKeyDown`, `char`, `keyUp`, the text always arrives in full; the page can still miss the very first `keydown` event. The tools type that way. |
| `Input.insertText` right after `Page.navigate` | Reaches the focused field in full (7/7 characters into a password field). `fill`'s replace, the `selectAll` editing command then `insertText`, works too. |
| The foreground tab, 20 tabs | `document.visibilityState` (read in an isolated world) is `visible` in exactly one tab: the one created last, or the one `Target.activateTarget` named. Reading all 20 takes 30–80 ms on one connection. A popup window (`window.open` with features) is `visible` and its opener turns `hidden`; a `target=_blank` link opened from a CDP click left its opener in front. Measured without a window manager, and the same again under PROJ-29's JWM (2026-11-14, `BROWSERPIER_SPIKE_ONLY=tabs`; 60–120 ms to read all 20 on a busier machine). |
| Large pages (AX tree / DOM snapshot) | 4,000 elements: 4.0 MB in 0.3 s / 0.9 MB in 0.1 s. 20,000: 20 MB in 2.0 s / 4.9 MB in 0.4 s. 80,000: 81 MB in 28 s / 20 MB in 1.7 s. 200,000: neither answered within 60 s. A real long page (Wikipedia's population list, 10,000 elements): 8.0 MB in 0.5 s / 3.4 MB in 0.3 s. The HTML spec (340,000 elements): no AX tree in 60 s; DOM snapshot 99 MB in 9 s. **The cap:** the tools take the DOM snapshot first, and a page with more than 60,000 DOM nodes (about 30,000 elements) is too large for the AX tree; `snapshot` says so and `find` reads the DOM snapshot's text alone. Messages are capped at 32 MiB, and at most two whole-page reads run at once. |
| Isolated-world build-up | 1,000 calls on one document (each: dial, attach, `getFrameTree`, `createIsolatedWorld`, `evaluate`, close) got the **same** execution context every time (Chromium keeps one world per name and document), with no measurable growth in renderer memory (1,876 → 1,901 MB on an already-large renderer, within noise). About 6 ms of overhead per call. |
| Anything left attached | After the connection closes, `Target.getTargets` shows the page `attached: false`. |

### The continuous detector

`browsertest.Watch` (`internal/browsertest/watch.go`) is a page that keeps checking, every 200 ms on its main thread and in a worker, for what pages use to spot CDP automation: the `Error.stack` console leak of an enabled `Runtime`, a logged `Proxy` being inspected, console calls getting slow, `navigator.webdriver` and automation globals; hooks on the DOM methods automation calls in the page's own world (`querySelector`, `getBoundingClientRect`, `focus`, `elementFromPoint`…); and input tells (screen position equal to client position, a click with no mouse movement, a key event without a code, text with no key pressed). It is shared with PROJ-57.

Console calls getting slow (`console-slow`: 50 `console.debug` calls costing five times what they did at load, and over 50 ms) is timing, so a starved machine trips it as well: on CI runners and on a workstation with its CPUs busy, the page's checks took 5–30 ms now and then, with or without calls. The tests run each batch through `Watch.Quiet`, which, when a batch's only hits are `console-slow`, runs a control batch of as many sleeps with no calls on the same page, and runs the batch again if the control is quiet; slow in the control too, with nothing attached, is the machine's. Measured on Chromium 156, `Runtime.enable` held through 20 calls doesn't make the checks slow at all, so the 50 ms floor (5 ms before 2026-11-14) costs no detection that the positive control shows.

**Positive control** (`browsertest.WatchPositiveControl`: what Playwright does when it connects and acts): detected, through main-world calls (`querySelector`, `getBoundingClientRect`, `focus`), its binding (`__playwright__binding__`), a click with no movement and text with no key pressed. `Runtime.enable` **alone** is no longer detected by the console checks on Chromium 156 (the `Error.stack` leak is closed; getters on an Error's `message` or `name` can't be used, as Chromium's console reads them with nothing attached).

**The tools' CDP calls, 20 runs each, nothing else attached:** nothing recorded, for reading (frame tree, AX tree, partial AX tree, DOM snapshot, isolated world), clicking (scroll into view, quads, hit-test in an isolated world, mouse move, press and release), `fill` (focus, `selectAll`, `insertText`), `type` (focus, key events) and tabs (targets, visibility of each).

**Accepted residuals** (each is what Playwright does as well, and API mode is the mode that isn't for protected sites):
- `fill` puts its text in with one `insertText` after a select-all key press: a detector that checks that typed text matches the keys pressed can tell. Typing with `type` sends one key per character.
- Key events come as fast as CDP sends them, with no typing rhythm.

**The tools themselves, 20 runs each** (`TestPageToolsStayQuiet`, `internal/api/page_tabs_docker_test.go`, in `make test-docker`), nothing else attached: nothing recorded for `snapshot`, `find`, `act` (click, fill, type, press and hover, each its own run), `wait_for` (text, and a ref), `tabs` and `navigate` (reload), while the positive control in the same test was caught (main-world calls and its binding). So there is no hit to decide on; the residuals above still stand.

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
