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
| [bot.sannysoft.com](https://bot.sannysoft.com/) | All pass except **WebGL renderer: SwiftShader**; all pass since WebGL moved to llvmpipe (PROJ-148: measured from a residential connection, not this OVH address; see below) | Same |
| [BrowserScan bot detection](https://www.browserscan.net/bot-detection) | **Normal** (webdriver, user agent, CDP, navigator) | **Normal**: its CDP check doesn't catch Playwright on Chromium 156 |
| [deviceandbrowserinfo: Are you a bot?](https://deviceandbrowserinfo.com/are_you_a_bot) | **"You are human!"**, every signal false | **"You are a bot!"**: `isAutomatedWithCDP` (main thread and workers) |
| [CreepJS](https://abrahamjuliot.github.io/creepjs/) | 0% headless, 0% stealth, 50% "like headless"; 44% since PROJ-29's taskbar, 38% since PROJ-148's llvmpipe (residential connection, not this OVH address; see below) | Same |
| [nowsecure.nl](https://nowsecure.nl/), [scrapingcourse.com Cloudflare challenge](https://www.scrapingcourse.com/cloudflare-challenge) | Cloudflare's interactive checkbox | Same |

**How to read these:**
- **CDP is the big difference between the modes**, as SPEC §10 expects. Pages that look for a CDP client find one in API mode and nothing in Visual mode. Use Visual mode for protected sites.
- **Cloudflare challenged both modes** from this datacenter IP. Clicking through it automatically wasn't attempted: that's what handoffs are for. A person finishes it from the handoff link, and the profile keeps the clearance cookie. From a residential connection (a home server, or a residential proxy), fewer challenges are expected.
- **With location matching off,** the browser ran in UTC with English, and deviceandbrowserinfo flagged that combination as a weak bot signal, even in Visual mode. Keep `BROWSERPIER_GEOIP_URL` on, or set a profile's time zone and language by hand.

**Rechecked after PROJ-26,** which sends every connection through the forwarder in the container (no QUIC, and WebRTC over TCP only): Visual mode is still "You are human!" on deviceandbrowserinfo, and sannysoft is unchanged.

## Signals that remain

None of these is spoofed, by design (SPEC §10). They come from the container:

- **Software WebGL renderer.** There's no GPU in the container, so WebGL is software-rendered and says so: `ANGLE (Mesa, llvmpipe (LLVM 19.1.7 256 bits), OpenGL 4.5)` since PROJ-148, which is what a Linux desktop or VM without a GPU driver reports. That is no longer SwiftShader, which was the one red line on sannysoft and an item of CreepJS's "like headless", but a detector can still read llvmpipe as "a VM, or no GPU". Giving containers the host's GPU would replace it where there is one (PROJ-30).
- **A 0×0 window, often, as a page starts.** In both modes, a page's first inline script often sees `outerWidth` and `outerHeight` as 0, which headless browsers report. It reads normally once the page has loaded. A window manager didn't change it (PROJ-29, below). The likely cause, from reading Chromium's source and not verified: a new document's widget hears its window's bounds from the browser only once it's shown, after the commit, while the renderer may already be running the page's first script (`RenderWidgetHostImpl::SendScreenRects` skips hidden widgets). If so, desktop Chrome does the same. PROJ-65 checks both.
- **No speech voices** (`speechSynthesis.getVoices()` is empty), as on many Linux desktops.
- **Back never restores from the back/forward cache, and says nothing about why** (PROJ-160, for memory: Memory flags, below). On every Back, every page is loaded again (`persisted` false, a `back_forward` navigation), and `performance.getEntriesByType('navigation')[0].notRestoredReasons` is `null`, whatever the page. With the cache on, Chrome restores a page it kept (`null` too, as the entry is the first load's) and gives a page it didn't keep an object: `{"reasons": [{"reason": "masked"}]}` for one with an `unload` handler, and for one dropped after its time limit. So a page that reads `notRestoredReasons` after a Back that loaded it again sees `null` where a person's Chrome, with its cache on, would give an object: what the cache being off looks like, which a person's browser rarely is. Any page can make that check, not only one the cache could keep; it takes a Back to the page and a script that reads it. Accepted, as decided in Memory flags below: the memory matters more to an agent than an instant Back, and nothing is spoofed (SPEC §10).

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

So the taskbar hint is gone, and the 0×0 at load is unchanged (see Signals that remain). On CreepJS, in Visual mode on this machine, "like headless" went from 50% to 44%: `noTaskbar` is now false. Of what it still counts, SwiftShader was the container's (gone since PROJ-148, below); the rest (`noWebShare`, `noContentIndex`, `noContactsManager`, `noDownlinkMax`: APIs desktop Linux Chromium doesn't have, and the default light theme) aren't about the container. The rate depends on timing: these runs, on 2026-11-12, shared 6 CPUs with other work, and PROJ-15 saw about a third.

## WebGL on llvmpipe (PROJ-148)

WebGL used to run on SwiftShader (`--use-angle=swiftshader --enable-unsafe-swiftshader`), whose renderer string pages read as a headless or server sign. It now runs on **Mesa's llvmpipe**, the renderer of a Linux desktop or VM without a GPU driver. It is still software rendering, and the string says so; it's not spoofed, and there's no fallback.

Measured on 2026-11-17 with Chromium 156.0.8078.79, Debian trixie's Mesa 25.0.7-2+deb13u1 and LLVM 19.1.7, on amd64 only: a 6-vCPU VM on an AMD Ryzen 9 6900HX, under rootless Podman. Both renderers ran on the same host, the same day, through the same **residential** connection (not PROJ-15's OVH datacenter address), with exit-location matching on. The SwiftShader runs used the image before the change, the same Chromium; flags were swapped with a throwaway wrapper during the spike.

**The switches.** `--ignore-gpu-blocklist` alone, with no `--use-angle`, puts WebGL on llvmpipe: ANGLE's default backend on Linux is OpenGL, through Xvfb's GLX (`chrome://gpu`: `GL implementation parts (gl=egl-angle,angle=opengl)`, display type `ANGLE_OPENGL`). No EGL package is needed. Without that switch Chromium blocks llvmpipe: `chrome://gpu` says `WebGL: Disabled`, `getContext('webgl')` returns null, and compositing falls back to software; so that switch is required, and the tests fail with "no WebGL" without it. `--disable-gpu-compositing` keeps page compositing in software (see the cost below); WebGL then reads `Hardware accelerated but at reduced performance` in `chrome://gpu`, and renders as before.

**Packages: none new, and the image is the same size.** `xvfb` already pulls in Mesa's GLX and llvmpipe (`xvfb` → `libgl1` → `libglx0` → `libglx-mesa0` → `mesa-libgallium`, 41 MB, which links `libllvm19`, 124 MB). The Dockerfile now names `libgl1` and `libglx-mesa0`, which Chromium needs, and the build fails if libGL, the Mesa GLX library or the gallium driver is missing, lacks llvmpipe or can't resolve its libraries. That checks the files and their linkage, not an actual load: the image has no GL client to load the driver with at build time (Xvfb reports nothing about GLX as it starts), so the load is checked by the Docker tests' `webgl` subtest. amd64 image: 1,100,647,500 bytes before, 1,100,647,108 after. arm64: not measured here; the dependency chain is the same in Debian.

**The renderer string**, exactly, from `UNMASKED_VENDOR_WEBGL` / `UNMASKED_RENDERER_WEBGL`:

| | Vendor | Renderer |
|---|---|---|
| Before (SwiftShader) | `Google Inc. (Google)` | `ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero) (0x0000C0DE)), SwiftShader driver)` |
| After, amd64 (AVX2) | `Google Inc. (Mesa)` | `ANGLE (Mesa, llvmpipe (LLVM 19.1.7 256 bits), OpenGL 4.5)` |
| After, arm64 | not run here | expected `… (LLVM 19.1.7 128 bits) …`, unchecked |

What varies: the LLVM version (and Mesa's, in `chrome://gpu`) changes with the image's Debian packages, and the vector width comes from the host CPU (256 bits with AVX2, 128 without it, as with SSE4 or NEON). So the string is the same for every profile on a host and image (`TestWebGLRendererIsStable`), changes once when an image update moves LLVM, and can change when a profile moves to a host with a different CPU. WebGL parameters change with it (`MAX_TEXTURE_SIZE` 16384 instead of 8192, line and point ranges, extension counts), and so do WebGL and canvas hashes, once, at the update.

**WebGL1 and WebGL2** both give a context and draw a triangle that reads back exactly (`51,204,102,255` with `readPixels`), and a WebGL canvas shows on the X display, which screenshots and the live stream read, with or without `--disable-gpu-compositing` (`TestBrowserEnvironment/webgl` and `webgl on screen`).

**Sandbox.** `chrome://sandbox` still says "You are adequately sandboxed" (namespaces and seccomp-BPF). `seccomp.json` and the AppArmor profile are unchanged, and llvmpipe's JIT works under BrowserPier's seccomp profile (the AppArmor profiles weren't checked, see below). The GPU process is in a process of its own (`In-process GPU: false`) and has never crashed in these runs. `chrome://gpu` said `Sandboxed: false` for it, **with SwiftShader too**: its `/proc` status showed only the container's seccomp filter (`Seccomp_filters: 1`), where renderers have Chromium's as well (2). Nothing in PROJ-148 changed that; PROJ-165 did (below). AppArmor: this rootless Podman has none (it refuses `apparmor=browserpier`; `unconfined` is the same as its default), so only "no AppArmor" ran here; the `browserpier` and Docker-default choices weren't.

**Cost**, `TestMeasureWebGLCost` (`go test -tags 'docker measure' -run TestMeasureWebGLCost -v ./images/chromium/`), a 1280×800 screen, 20-second samples of the container's cgroup (memory as `docker stats` counts it, which the RAM limit uses; CPU in percent of one core). The WebGL page redraws 40 large, blended, full-window triangles a frame; the heavy site is a long Wikipedia article scrolled through Visual mode every half second; "watched" reads the live stream at the same time.

| | SwiftShader | llvmpipe, GPU compositing | llvmpipe, `--disable-gpu-compositing` (chosen) |
|---|---|---|---|
| First page loaded, from `docker run` | 1.5–2.0 s | 1.2–1.6 s | 1.6–1.8 s |
| Idle page: CPU, memory | 7–8%, 238–240 MB | 7–8%, 242–246 MB | 7%, 220–222 MB |
| WebGL page | 48 fps, 376% CPU, 314–321 MB | 62 fps, 274% CPU, 300 MB | 63 fps, 240–281% CPU, 294–299 MB |
| WebGL page, watched | 386% CPU, 397–401 MB | 297% CPU, 385 MB | 265–300% CPU, 385–387 MB |
| Heavy site, scrolled | 83–84% CPU, 390–401 MB | 34–35% CPU, 379–381 MB | 20% CPU, 342–381 MB |
| Heavy site, watched | 83–85% CPU, 484–487 MB | 47–48% CPU, 472–503 MB | 34–37% CPU, 425–476 MB |
| GPU process | 120–185 MB, 20–22 threads | 197–252 MB, 25–26 threads | 159–239 MB, 24 threads |

The frame rates here were counted over the 20-second window plus the readings that followed it (a second or more), so they read high: the 63 fps is the 60 fps frame cap. Every column was read the same way, so the comparison holds. The measure now times the window alone (after the PR #79 review).

With 3,000 triangles every core is busy either way: SwiftShader drew 2 frames a second, llvmpipe 4. So llvmpipe draws WebGL faster for less CPU, and the container's memory stays where it was, well inside SPEC §3's 1–2 GB a browser. Compositing pages in software as well cut the CPU of scrolling an ordinary page by about 40% against compositing them through llvmpipe (and by three quarters against SwiftShader, which composited every page), so the image passes `--disable-gpu-compositing` too.

**Threads.** llvmpipe starts one rasteriser thread per CPU the process may run on (6 here; 2 with Chromium pinned to two CPUs), where SwiftShader started 6 whatever the pinning. Pinned to two CPUs, llvmpipe drew the WebGL page at 60 fps for 183% CPU, SwiftShader at 24 fps for 187%; `LP_NUM_THREADS=1` cut llvmpipe to 36 fps and saved 7 MB. The threads sleep unless a page draws WebGL, and a container's CPU limit caps what they can take in total, so PROJ-148 left `LP_NUM_THREADS` unset; PROJ-165 sets it to 0 for the GPU sandbox (below). It doesn't show in the renderer string; a page sees only `navigator.hardwareConcurrency`, which follows the CPUs and not llvmpipe, and how fast it draws.

**Detection pages**, Visual mode (the address bar through xdotool, screenshots; each page's text read over CDP only after it had shown its results), same connection, before and after:

| Page | SwiftShader | llvmpipe |
|---|---|---|
| bot.sannysoft.com | All pass except **WebGL renderer** (it fails anything containing "Swift") | **All pass**, the 19 fingerprint-scanner checks too |
| CreepJS | 0% headless, 0% stealth, **44% like headless** (`hasSwiftShader: true`); GPU confidence "moderate" | 0% headless, 0% stealth, **38% like headless** (`hasSwiftShader: false`); GPU confidence "high" |
| BrowserScan bot detection | Normal | Normal |
| BrowserScan fingerprint | Authenticity 100%, bot detection No | Authenticity 100%, bot detection No |
| deviceandbrowserinfo: Are you a bot? | "You are human!", every signal false | "You are human!", every signal false, `isWebGLInconsistent` and `hasInconsistentGPUFeatures` included |

What CreepJS still counts in "like headless" is `noWebShare`, `noContentIndex`, `noContactsManager`, `noDownlinkMax` (APIs desktop Linux Chromium doesn't have) and the default light theme (`hasKnownBgColor`, `prefersLightColor`); none comes from the container now. **Decision: go**, on amd64: sannysoft's WebGL line and CreepJS improved, nothing got worse, and it costs less.

**Existing profiles.** `TestLoginSurvivesImageUpgrade` signed in with the 0.2.1 image (SwiftShader), then started that profile, its `GPUCache` folders included, on the new image: it kept the login, WebGL worked on llvmpipe, and the GPU process didn't fail. The renderer, and with it WebGL and canvas hashes, changes once for such a profile.

**Not run here, so unverified:** arm64 (CI builds the arm64 image under emulation, and the build-time library check runs there; no Docker test has run on arm64, so WebGL on llvmpipe there is unchecked, and since there's no fallback, an arm64 failure would mean no WebGL at all: run `TestBrowserEnvironment` and `TestWebGLRendererIsStable` on an arm64 host or the Umbrel before a release ships this), the Umbrel, llvmpipe's JIT under the `browserpier` and Docker-default AppArmor profiles (CI's Docker job, on Docker with AppArmor, covers one of them once the branch runs there), and PROJ-15's OVH datacenter address. The **go** above is for amd64 without AppArmor.

`TestVisualModeLooksLikeAPerson`, `TestPageToolsStayQuiet`, `TestContentStaysQuiet`, the login tests and `TestMeasureWindowAtLoad` (window 1280×768 and `availHeight` 768 in all 70 loads, as in PROJ-29's table) pass unchanged.

## The GPU process's own sandbox (PROJ-165)

Until PROJ-165 the GPU process, which handles pages' WebGL calls, shaders and compositing, ran under the container's seccomp filter, with no capabilities and no new privileges, but without Chromium's own GPU seccomp policy: `chrome://gpu` said `Sandboxed: false`, with SwiftShader as with llvmpipe. Measured on 2026-11-18, same host and Chromium 156.0.8078.79 as above.

**Why.** On Linux Chromium starts the GPU process's sandbox after GL has started (the late start is its default outside ChromeOS), and `SandboxLinux::InitializeSandbox` can only start it in a single-threaded process. In a GPU process with other threads it logs `WARNING:sandbox/policy/linux/sandbox_linux.cc:405] InitializeSandbox() called with multiple threads in process gpu-process.` and goes on without it; for every other process type that's fatal. Here GL starting meant Mesa starting threads: llvmpipe's rasterisers (`llvmpipe-0` to `llvmpipe-5`, one per CPU) and the shader disk cache's writer (`chromiu:disk$0`). SwiftShader, before PROJ-148, started its own worker threads the same way. So it isn't Debian's build, the zygote, the container's `seccomp.json`, `/dev/shm` or the missing `/dev/dri`: a Linux desktop whose Mesa driver starts threads as GL starts would be the same (not checked on a desktop VM here; the code path is Chromium's own).

**What was tried**, each with a second Chromium in a running container, its GPU process's threads and `Seccomp_filters` read from `/proc`:

| Chromium's environment and switches | GPU process | WebGL |
|---|---|---|
| as before | 24 threads, `Seccomp_filters: 1`, the warning above | llvmpipe |
| `LP_NUM_THREADS=0` | 12 threads (the disk-cache one still there), 1 filter, the warning | llvmpipe |
| `LP_NUM_THREADS=0 MESA_SHADER_CACHE_DISABLE=true` (chosen) | 11 threads, **2 filters**, `Activated seccomp-bpf sandbox for process type: gpu-process` | llvmpipe, draws and reads back |
| `--gpu-sandbox-start-early` | sandboxed before GL, then `ANGLE Display::initialize error 12289: Could not open the default X display`, GL fails | none |
| `--gpu-sandbox-failures-fatal=yes`, no settings | `FATAL: Current process is not mono-threaded!` after 30 tries, three crashes, then a GPU process without GL, sandboxed | none |

Starting the sandbox early can't work without widening Chromium's GPU policy, which is Chromium's code: ANGLE opens its own connection to the X display as GL starts, and the sandbox no longer lets it.

**What the image does.** The entrypoint gives Chromium `LP_NUM_THREADS=0` (llvmpipe rasterises on the GL thread itself) and `MESA_SHADER_CACHE_DISABLE=true` (Mesa's on-disk shader cache sat under the container's `HOME`, lost with the container, and the sandbox would keep it from opening its files; Chromium's own `GPUCache` in the profile is unchanged), and passes `--gpu-sandbox-failures-fatal=yes`, so a GPU process that can't sandbox itself exits instead of running on: after its restarts Chromium falls back to a GPU process without GL, sandboxed, and pages get no WebGL. The image build fails if Mesa's `libgallium` doesn't know either setting (their names aren't in it); whether they work, the Docker test shows. If the sandbox fails at run time anyway, the entrypoint logs one warning, `a GPU process exited because it couldn't start its sandbox; Chromium restarts it, and if that keeps failing, pages get no WebGL`. It waits for the FATAL `Current process is not mono-threaded!` that follows Chromium's `InitializeSandbox() called with multiple threads in process gpu-process` line, since Chromium first waits for the threads to end and the sandbox may then start after all, so the admin reads it in the browser's log; nothing attaches to the page for it. `seccomp.json` and the AppArmor profile are unchanged, and llvmpipe's JIT works inside Chromium's GPU policy. `chrome://gpu` now says `Sandboxed: true`, the GPU process's `/proc` status `Seccomp_filters: 2`, and `TestBrowserEnvironment/gpu` requires both; the Docker tests fail on the warning above too.

**Fingerprint.** Unchanged: the renderer string, WebGL parameters and extensions don't depend on llvmpipe's thread count, and `navigator.hardwareConcurrency` follows the CPUs. A page could only see that WebGL draws more slowly.

**Cost**, `TestMeasureWebGLCost` with the same 40-triangle WebGL page (light: the old setup had cores to spare, so its 63 fps is the frame cap) and with 3,000 triangles (`MEASURE_TRIANGLES=3000`, which kept every core busy before), before and after:

| | Before (llvmpipe, threads) | After (one thread, sandboxed) |
|---|---|---|
| First page loaded, from `docker run` | 1.9 s | 1.2–1.8 s |
| Idle page: CPU, memory | 7%, 220 MB | 7%, 222 MB |
| WebGL page | 63 fps, 244% CPU, 297 MB | 29–34 fps, 117–119% CPU, 289–290 MB |
| WebGL page, watched | 268% CPU | 139–141% CPU |
| WebGL page, 3,000 triangles | 4.2 fps, 568% CPU | 0.8 fps, 107% CPU |
| … watched | 560% CPU | 125% CPU |
| Heavy site, scrolled | 19% CPU | 19–20% CPU |
| Heavy site, watched | 35% CPU | 36% CPU |
| GPU process | 24 threads | 11 threads |

These frame rates were read the way the PROJ-148 table's were, so they too read a little high (63 fps is the 60 fps cap); before and after were read alike.

WebGL gets one core instead of every core. On the light page that halves the frame rate and the CPU; on a scene that used every core it is about as many times slower as the host has cores (5 times here, on 6; more on a bigger host), for a fifth of the CPU. That also leaves the other cores to the host's other browsers. Ordinary pages, which are composited in software and don't touch llvmpipe, cost the same. **Decision: go**: the GPU process parses what pages send it, and AGENTS.md's rule is that Chromium keeps its sandbox.

**Not run here:** **arm64** can't run on this amd64 VM, so whether Mesa on arm64 starts no thread before the sandbox is unchecked. If it does start one, `--gpu-sandbox-failures-fatal` means pages there get no WebGL at all rather than an unsandboxed GPU process; CI builds the arm64 image (and its library check) under emulation but runs no Docker test on it. Run `TestBrowserEnvironment` and `TestWebGLRendererIsStable` on an arm64 host (the Umbrel, if it's arm64) before a release ships this: PROJ-187, with PROJ-148's arm64 check. Also not run: the Umbrel, the `browserpier` and Docker-default AppArmor profiles (this rootless Podman has no AppArmor), as for PROJ-148; and a desktop VM with Debian's Chromium on llvmpipe, to compare. `TestBrowserEnvironment`, `TestWebGLRendererIsStable`, the login tests, `TestLoginSurvivesImageUpgrade` from the 0.3.0 image (SwiftShader), `TestSeccompAllowsOnlySandboxNamespaces` and `TestAppArmorProfile` pass.

## Memory flags (PROJ-160)

Two Chromium features keep memory for speed: the **back/forward cache** keeps a page the tab has left whole in memory, to show it at once on Back, and the **spare renderer** keeps a renderer process started for the next site. Browser containers have no memory cap of their own, and admission counts what each uses (SPEC §3, Capacity), so a page kept for a Back nobody makes can get another profile's idle browser stopped, or a start refused. easi.run turns both off on its numbers; these are ours.

Measured on 2026-11-17 with `TestMeasureMemoryFlags` (`go test -tags 'docker measure' -run TestMeasureMemoryFlags -v -timeout 90m ./images/chromium/`), Chromium 156.0.8078.79, amd64, on the PROJ-148 image (WebGL on llvmpipe, pages composited in software): a 6-vCPU VM on an AMD Ryzen 9 6900HX, rootless Podman 5.7, a 1280×800 screen, one container at a time. Memory is the container's as admission counts it (`internal/docker/memusage`, which `MemoryUsage` uses: usage less the inactive page cache), in MiB. Each run sets Chromium's features itself through a throwaway wrapper mounted over its binary (the image's own binary, copied out): it drops the entrypoint's `--disable-features` and passes the run's own, so "cache on" is Chromium's defaults on any image. The tables below were measured on the PROJ-148 image, before this change, except where it says otherwise; the corrected test reproduced the cache-on and cache-off rows (Visual, other site, 30 s) on the new image on 2026-11-18: 491 and 211 MiB at +30 s. `/heavy` is a test-site page that holds 200 MB in typed arrays and a 20,000-paragraph DOM, and that the cache can keep: a GET, no `unload` handler, no `no-store`, nothing left open. Each page reports its `pageshow` to the test site (`persisted`, the navigation's type, `notRestoredReasons`, and a number random to each document).

**Back/forward cache.** The tab opens `/hello`, then `/heavy`, then `/hello` again; the memory is read every 5 s for 4 minutes, then the tab goes Back. Visual mode drives the address bar through xdotool, with no CDP client; API mode uses `Page.navigate` and `history.back()` with a CDP client attached throughout. "Other site" is `/hello` at another address of the test site, as an agent going from a search to a result.

| Mode | Leaves to | Cache | Before | On `/heavy` | +0 s | +15 s | +30 s | +1 min | +2 min | +4 min | Back to `/heavy` |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Visual | same site | on | 226 | 550 | 570 | 516 | 475 | 473 | 471 | 479 | restored (`persisted` true) |
| Visual | same site | **off** | 223 | 545 | 562 | 508 | 216 | 214 | 212 | 214 | loaded again |
| Visual | other site | on | 222 | 558 | 583 | 537 | 492 | 489 | 488 | 490 | restored |
| Visual | other site | **off** | 223 | 535 | 255 | 210 | 210 | 208 | 207 | 209 | loaded again |
| API | same site | on | 209 | 512 | 512 | 503 | 464 | 460 | 458 | 461 | restored |
| API | same site | **off** | 210 | 509 | 509 | 498 | 207 | 203 | 201 | 203 | loaded again |
| API | other site | on | 209 | 529 | 541 | 529 | 484 | 480 | 479 | 481 | restored |
| API | other site | **off** | 210 | 510 | 212 | 201 | 204 | 200 | 198 | 201 | loaded again |

With the cache on, 252–272 MiB of the page was still counted four minutes after the tab left it, in every case. With it off, the memory was back at its level before the page within 5 s when the tab went to another site (its renderer process exits), and within 25 s on the same site (the renderer is reused, and frees the page at its next garbage collection). An attached CDP client changed nothing the cache keeps: the API rows match the Visual ones. A longer run (Visual, other site, cache on, `MEASURE_SECONDS=660`) held the page's memory until 595 s after the tab left it, and dropped it by 600 s (489 → 209 MiB); Back then loaded the page again.

- **Chromium's limits**, from its source at this version (`content/browser/back_forward_cache/back_forward_cache_impl.cc`): a page stays at most 600 s (3 minutes for a `no-store` one; the 600 s is measured above), six pages at most; under memory pressure, three in the foreground and one in the background (moderate), none (critical).
- **Memory pressure.** easi.run's claim holds here: on Linux, the only memory-pressure source in this binary is `dbus_memory_pressure_evaluator_linux` (systemd-oomd or low-memory-monitor, over the system D-Bus). The container runs no system bus (there's no `/run/dbus`), so Chromium never hears memory is short, whatever the host has left, and only the time limit or the six-page limit drops a page. Not tested by driving the host short of memory: on a shared machine that risks the OOM killer for everything else.
- **The feature name.** `BackForwardCache` is in the binary as a whole string, and the switch takes effect (`persisted` false). `--disable-features` puts no bar over the page: `TestBrowserEnvironment`'s "no warning bar over the page" passes on the new image (the managed policy hides the unsupported-flag bar, as for `--disable-blink-features`).
- **One `--disable-features`.** With two, Chromium used only the last: `--disable-features=BackForwardCache --disable-features=Translate` left the cache on (A restored on Back), the other order turned it off. So the image passes one, with every feature in it.

**Decision: off** (the ticket's rule: at least 100 MB still counted 60 s after leaving with it on, and back within 30 s with it off; both hold in all four cases). `--disable-features=BackForwardCache`, in `flags.go`, the Dockerfile's feature check and SPEC §3.

**What a page sees.** Back to a page loads it again: `pageshow` with `persisted` false, a new document, and a navigation of type `back_forward`. Its HTML came from the HTTP cache (the test site wasn't asked again) unless it was served with `no-store`, as Back does in any browser when it can. `notRestoredReasons`, Back from `/back/b` in Visual mode, the "reasons" part of the test, on the new image (2026-11-18):

| Page | Cache on (Chromium's defaults) | Cache off |
|---|---|---|
| `/back/a`, one the cache can keep | restored (`persisted` true); `null` | loaded again; `null` |
| `/back/unload`, an `unload` handler | loaded again; `{"reasons": [{"reason": "masked"}], …}` | loaded again; `null` |
| `/back/no-store`, `Cache-Control: no-store` | restored (`persisted` true): Chromium 156 keeps such pages (for 3 minutes); `null` | loaded again, asked of the site again; `null` |
| `/heavy`, dropped after its 600 s (the long run above) | loaded again; `{"reasons": [{"reason": "masked"}], …}` | — |

So the flag isn't named (Chrome masks the reasons it gives pages), but with the cache off every page loaded again on Back gets `null`, where with it on a page the cache didn't restore gets an object. That difference is visible to any page, cache-eligible or not; see Signals that remain.

**Spare renderer** (`SpareRendererForSitePerProcess`): a browser idle 60 s on `/hello`, three runs each; first on the PROJ-148 image with the back/forward cache on, then (2026-11-18) on the new image with it off in both, as the image runs.

| | Run 1 | Run 2 | Run 3 | Renderer processes |
|---|---|---|---|---|
| Cache on, spare on (PROJ-148 image) | 205 | 206 | 206 | 4 (353 MiB RSS together, much of it shared) |
| Cache on, spare off | 199 | 196 | 197 | 3 (278 MiB) |
| Cache off, spare on (`BackForwardCache`) | 207 | 205 | 207 | 4 (353–354 MiB) |
| Cache off, spare off (`BackForwardCache,SpareRendererForSitePerProcess`) | 198 | 197 | 196 | 3 (266–279 MiB) |

The renderer column counted the measuring shell too, as its command line named the pattern it looked for: one process and a few MiB too many in every row. The memory columns come from the cgroup and are unaffected. The measure now counts Chromium's processes only (after the PR #82 review).

Its name is in this Chromium only as `kSpareRendererForSitePerProcess`: Chromium's `BASE_FEATURE` now names a feature from its constant, reading the string from its second character, so a whole-string search for the name finds nothing though the feature is there, and turning it off did drop a renderer. It saves 6–11 MiB, as easi.run measured, under the 20 MiB threshold: **left on**. (The Dockerfile's feature check lists each feature in the one form the binary stores it in; `BackForwardCache` is stored whole. A misspelt name fails the build: checked with `BackForwardCach`.)

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
- **The address goes in at 12 ms a key**, faster than anyone types, at both paces. No page sees those keys, only the address bar; nor does the search engine's suggest service, since the image turns suggestions off (PROJ-191, below): without that, an `open` of an `http://` address sent its path to it, token included.
- **The layout is Chromium 156's.** The address bar's place is computed (y = 63, the middle of x = 121 to width − 85), not found on the screen. On another Chromium `open` refuses (409) rather than click blind, and `TestOpenAtEveryScreenSize` fails until it is measured again.
- **A window that opens between a look and the run after it.** Each look at the screen and the xdotool run it allows are separate: a pop-up opening in those few milliseconds would still take the press (the click, Delete or Return) or up to 64 characters of the address; the next look stops the rest. The pauses all come before a look, never between a look and its press, so this window stays a few milliseconds.

## No address-bar search suggestions (PROJ-191)

With suggestions on (Chromium's default), the address bar sent the default search engine what was being typed, a request per pause: a word in full, an `http://` address or one without a scheme up to its query, path and token included, and an `https://` address's host only; a query (`?token=…`) never went, and focusing the address bar sent nothing. Measured with `TestMeasureSuggest` (`go test -tags 'docker measure' -run TestMeasureSuggest -v ./images/chromium/`, 2026-11-18, Chromium 156), which makes the search engine a local one by a test policy and sends everything else to an isolated proxy:

| Typed with `key ctrl+l` and `type` (instant pace / human pace) | Suggest requests | The secret in them |
|---|---|---|
| `pierword` | 2 / 8 | (a word) |
| `http://site.test/reset?token=SECRET` | 2 / 23 | none: they stop at the `?` |
| `http://site.test/reset/SECRET` | 4 / 35 | all of it |
| `https://site.test/reset?token=SECRET`, `https://site.test/reset/SECRET` | 2 / 17 | none: the host only |
| `site.test/login/SECRET` | 3 / 27–28 | all of it |
| With the image's own engine, `http://site.test/reset/SECRET` | 4–5 / 35–38 attempts to connect to `duckduckgo.com` (counted at the proxy), over three runs | (TLS: not readable) |

The image now sets `SearchSuggestEnabled: false`. In the same runs, typing sent the suggestion service no request before Return, and the browser asked for no `duckduckgo.com` while an address was typed (`TestTypedAddressReachesNoSuggestService` checks both, and that the address then opens exactly as typed). The test also opens the two `http://` addresses with PROJ-164's `open` action, which types into the same address bar: without the policy it sent 3–4 suggest requests at either pace, the path's token included, and with it none (2026-11-26). A word still goes to the engine as a search once Return is pressed; the test checks that too, which also shows its search engine is in effect.

What can see it:
- **A person**, in the live view or a handoff: the address bar's drop-down no longer offers the engine's completions. History and bookmarks still do. The setting shows on `chrome://policy`, next to the image's other policies, and Chromium already says it's managed.
- **Not pages.** No web API reports it, and a page can't read `chrome://policy` (a `fetch` of it fails with a TypeError, as in any Chromium, with or without the policy; the test records it). Nothing about the browser's fingerprint changes.
- **The search engine** sees searches that come without the suggest requests that usually precede them, as from anyone who turned suggestions off (a common privacy setting) or pasted the words. That's DuckDuckGo, not the sites an agent works on.

Considered and left as they are:
- **`NetworkPredictionOptions`** (preloading): no HTTP request reached the typed address's host before Return in any of the runs (connections weren't timed, so a bare preconnect wouldn't show), and turning prediction off would change what pages' speculation rules get, which a page can notice. Kept at Chromium's default.
- **`UrlKeyedAnonymizedDataCollectionEnabled`**: off by default in Chromium, and it reports to Google only for a signed-in, synced profile. Nothing to change.
- **The start page:** a new profile's first tab still opens `start.duckduckgo.com` (PROJ-127, separate). The runs also saw Chromium ask for `accounts.google.com`, `update.googleapis.com` and `connectivitycheck.gstatic.com` on its own (refused by the test's proxy, so not measured further; PROJ-192).

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

## The file chooser portal (PROJ-152)

Spike and measurements, 2026-11-18, Chromium 156.0.8078.79 (Debian trixie), dbus-daemon 1.16.2, amd64, in the real browser container (Xvfb, JWM, BrowserPier's seccomp profile, the AppArmor choice this host gets: Docker's default under rootless Podman 5, sandbox on), host kernel 7.0. Spike variants were the 0.3.0 image with a logging portal and `dbus-monitor` on the bus (never merged); the numbers marked *final* are from this branch's image. Every page was opened through the address bar with xdotool. **Decision: go**, with the focus move of point 4 and the residual of point 6. PROJ-152 shipped it off by default behind `BROWSERPIER_FILE_CHOOSER_PORTAL` until the person's picker existed; PROJ-181 (2026-11-18) added the picker, turned the portal on in every browser, removed the setting, and made a browser whose bus or portal can't start exit instead of falling back to the GTK dialog (`TestNoPortalNoBrowser`), so PROJ-183's hole has no way back (`TestChooserTakesNoTypedPath`).

1. **Chromium uses the portal under X11 with no switch.** It needs a session bus in `DBUS_SESSION_BUS_ADDRESS` and `org.freedesktop.portal.Desktop` owned on it **before it starts**: as it starts it asks `NameHasOwner`, `ListActivatableNames` and the `version` property of `org.freedesktop.portal.FileChooser` (3 or more; the portal reports 4, which has `directory`), and from then on every file chooser (`<input type=file>`, a script's `input.click()`, `showOpenFilePicker`, `showSaveFilePicker`, ctrl+S) is an `OpenFile` or `SaveFile` call, with no GTK dialog. `GTK_USE_PORTAL` plays no part, and no feature or switch was needed, so `chromiumArgs` is unchanged and a page has no switch to tell. With no bus (0.3.0), or the bus but not the name, the GTK dialog shows as before.
2. **What a request holds.** `OpenFile(parent_window "x11:0x…", title, options)`: `handle_token`, `modal: true`, `current_folder` (the last folder used, or `$HOME`), `multiple: true` for a `multiple` input or `showOpenFilePicker({multiple})`, `directory: true` for `webkitdirectory` (with `accept_label "Upload"`), and `filters`/`current_filter` from the input's `accept`: Chromium turns each MIME type into globs, case-blind (`image/*` became 20 patterns such as `*.[pP][nN][gG]`, `.pdf` one), and adds `All Files` (`*.*`). A `showOpenFilePicker` filter keeps the page's own description as its name; only the patterns are reported, cleaned, at most 32 of 64 bytes. `SaveFile`: `current_name` already cleaned by Chromium (`../evil name.txt` came as `_evil name.txt`), `current_folder`, and for ctrl+S the page's title as the name with filters for HTML only, single file (MHTML) and complete (the default). Chromium calls `Close` on the request object when the tab is closed or another tab comes to the front, and never otherwise; a page that navigates its own tab away was not measured (PROJ-182; its chooser ends at the 5-minute limit at worst). The caller is always the browser process itself.
3. **The window while a request waits.** Nothing shows on the screen. Chromium's whole window ignores input until the answer, as with its GTK dialog: clicks in the page, typing, ctrl+T, ctrl+W, F5 and clicks on the tab strip all did nothing. A tab opened over CDP meanwhile made Chromium `Close` the waiting request and the page got `cancel`; a chooser from that new tab was a new request. A graceful stop with a chooser open worked (`Browser.close`, exit 0). So an unanswered chooser freezes the browser for a person in the live view: hence the 5-minute limit, and a person's picker must ship with this (follow-up ticket).
4. **What the page sees** (`/chooser` page, `TestChooserAnsweredByAnAgentVisual`): the same `input` and `change` events as with the GTK dialog, both trusted, and `cancel` when cancelled, refused or timed out. One difference: the GTK dialog is a window of its own, so the page's window blurs as it opens and focuses again as it closes, which the portal alone doesn't cause (no blur, no focus). The entrypoint moves the X focus to the taskbar while a chooser waits and back to the browser's window when the last one ends (`focus.go`), so the page sees, for an answer: `blur` (the input, then the window), `input`, `change`, then the window's and the input's `focus`; for a cancel: the window's `blur`, `cancel`, `focus`. That is the GTK dialog's sequence, less the extra `blur`/`focus` pair JWM adds as the GTK dialog closes. A refused folder chooser answers at once, without the focus move: `cancel` only. `browsertest.Watch` (main thread and worker) over 20 rounds of a click on its file input and an answer from the uploads: nothing, with its positive control caught in the same run (`TestChooserStaysQuiet`). `TestVisualModeLooksLikeAPerson` and `TestPageToolsStayQuiet` pass unchanged.
5. **SaveFile.** Chromium doesn't need the answered file to exist: an answer naming a new file in `/files/downloads` works for ctrl+S (the page saved there, HTML only, as Chromium's choice when the answer names no filter) and for `showSaveFilePicker` (Chromium writes it through a `.crswap` file and renames it, so the page can write a file in the downloads folder, as a person's choice would let it). The portal still creates the file first, empty, exclusively and without following a link, so a name planted meanwhile can't be followed; Chromium then replaces it. Saves were too quick to see a `.crdownload` on the way. The PDF viewer's save button and ctrl+S on an unedited PDF download it without asking (the managed policy), so no `SaveFile` comes from them; an edited PDF's save wasn't tried.
6. **What else a session bus changes.** On the portal's name Chromium also asks `org.freedesktop.host.portal.Registry.Register`, `Settings.Read` (`color-scheme`, `accent-color`) and the `version` of `Notification` and `PowerProfileMonitor`: each gets D-Bus's standard unknown-interface error (`TestPortalOnARealBus`), and `prefers-color-scheme`, `prefers-contrast`, the fonts and `Notification.permission` read the same as on 0.3.0. On the bus it also asks `NameHasOwner` of systemd, the screen savers (GNOME, MATE, Cinnamon, Xfce, freedesktop), `org.freedesktop.Notifications` and `com.canonical.AppMenu.Registrar`; nobody owns them. GTK's accessibility bridge asks `org.a11y.Bus`, which the bus refuses (no service files; PROJ-150 points the bridge at its own bus with `AT_SPI_BUS_ADDRESS`, which skips that). `--password-store=basic` still decides the cookie key (no secret service is asked) and logins survive (`TestLoginSurvivesStopStartAndRebuild`, `TestAgentKeepsItsLoginThroughTheAPI`); the cross-image check by hand (a login made on 0.2.1, then this image, and back) wasn't run, for the same reason as PROJ-150's. Chromium's start log is shorter (its session-bus connection errors are gone); a cancelled chooser logs `Request cancelled by user.` **MPRIS:** when a page plays media, Chromium asks to own `org.mpris.MediaPlayer2.chromium.instanceN` (which would publish the page's media title on the bus); the bus lets only the portal's name be owned, so it is refused, Chromium logs `Failed to get the ownership of org.mpris…` once per media session and plays on (checked 2026-11-18 with an `<audio>` page). **Residual:** Chromium aborts when its session bus goes away (`D-Bus connection was disconnected. Aborting.`, measured by killing the bus), so the bus is as vital as Xvfb: it is stopped only after Chromium has closed, and if it ever dies the session ends as a crash, losing what Chromium hadn't flushed. Nothing in the container stops it.
7. **Caller attestation.** The image's dbus-daemon gives `ProcessFD` (a pidfd) on this host, kernel 7.0, rootless Podman included; it needs `SO_PEERPIDFD` (Linux 6.5). By kernel: Debian 13 (6.12) has it, Debian 12 (6.1) and umbrelOS 1.x (Debian 12's base) don't, and fall back to the pid and its start time, logged once (not run on those hosts here). From the entrypoint (PID 1, subreaper) the caller's chain is one step: the browser process, child of PID 1. A pidfd is closed after each check.
8. **Sandbox and reach.** `chrome://sandbox` still says "You are adequately sandboxed" (`TestBrowserEnvironment`). No change to `seccomp.json`, the AppArmor profile or the capabilities. The bus listens on one path socket, `/tmp/browserpier-bus-<uid>/bus`, in a folder that is 0700 and the browser's uid's own, with EXTERNAL authentication: no abstract socket and no TCP of its own (`TestSessionBus`, from the kernel's socket tables). Every renderer runs under a seccomp filter with its root an empty `/proc/<zygote>/fdinfo`, so no path reaches the socket (`TestSessionBus`). Checked under rootless Podman 5.
9. **Cost** (*final*, against 0.3.0, two runs each): an idle browser on a blank page 280–281 MB before, 283–284 MB after (`docker stats`); start to `/healthz` 0.8–1.8 s before and 1.3–1.8 s after (this VM's noise); the amd64 image 2.1 MB larger (the entrypoint with godbus; `dbus-daemon` was already pulled in by `chromium`); arm64 not built here.

**The GTK dialog browses the container** (checked on 0.3.0): driven with xdotool, it accepted `/profile/Local State` and the page got the file. With the portal no dialog exists: a chooser is answered only by names of files in `/files/uploads`, checked by the orchestrator (`os.Root`, no link) and again by the entrypoint (`lstat` in the folder), which builds the only URI that reaches Chromium, `file:///files/uploads/<name>` (`TestChooserOnlyFromUploads`, which fails with either check removed).

**Go:** Chromium uses the portal with nothing a page can see but what a desktop's dialog shows, the sandbox is untouched, and the cost is a few MB. **Pending:** the arm64 image, the public detector pages from PROJ-15's IP, the cross-image login check, and the Debian 12 and umbrelOS fallback on real hosts (PROJ-182).
