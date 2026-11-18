package main

import "fmt"

// panelHeight is the taskbar's height in pixels, which the window manager
// reserves at the bottom of the screen. It must match the tray in jwmrc.
const panelHeight = 32

// chromiumArgs is Chromium's command line. It never includes
// --enable-automation, --headless or --no-sandbox, and spoofs nothing.
// smallShm is set when /dev/shm is too small for Chromium's shared memory.
func chromiumArgs(c config, smallShm bool) []string {
	args := []string{
		"--user-data-dir=" + c.ProfileDir,
		// Chromium binds this to the container's loopback; the control socket relays it.
		"--remote-debugging-port=9222",
		// A fixed key, so cookies decrypt in every container and survive image updates.
		"--password-store=basic",
		"--disable-blink-features=AutomationControlled",
		// No GPU in the container: WebGL runs on Mesa's llvmpipe, through
		// ANGLE's default OpenGL backend over GLX, as on a Linux desktop
		// without a GPU driver. One renderer, no fallback: if llvmpipe can't
		// start, pages get no WebGL. Chromium blocks software GL, which
		// leaves no WebGL at all, unless told to ignore its blocklist.
		"--ignore-gpu-blocklist",
		// Pages are composited in software, as Chromium does on a desktop
		// whose GPU it blocks: cheaper than compositing every page through
		// llvmpipe, and WebGL still renders on it (ANTI-BOT.md, PROJ-148).
		"--disable-gpu-compositing",
		// Chromium starts the GPU process's own seccomp sandbox after GL
		// has started, and only if the process is still single-threaded
		// then; otherwise it runs on without it, under the container's
		// filter alone (PROJ-165). chromiumEnv keeps Mesa from starting
		// threads that early; this makes a GPU process that still has
		// them exit instead, so it never runs unsandboxed. If that kept
		// happening, Chromium would fall back to a GPU process without GL,
		// sandboxed, and pages would get no WebGL.
		"--gpu-sandbox-failures-fatal=yes",
		// Every connection goes through the forwarder, which refuses the
		// local network unless the profile allows it (egress.go). With a
		// single proxy and no DIRECT fallback, if it's down nothing goes out.
		"--proxy-server=socks5://" + forwarderAddr,
		// Chromium sends loopback and link-local addresses around the proxy
		// unless told not to; the metadata endpoint is link-local.
		"--proxy-bypass-list=<-loopback>",
		// Nothing resolves a name locally, so DNS can't go around it either.
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		// Nor can WebRTC's UDP, which the forwarder doesn't carry.
		"--webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--disable-component-update",
		"--disable-background-networking",
		"--no-first-run",
		"--no-default-browser-check",
		"--hide-crash-restore-bubble",
		"--noerrdialogs",
		// The window fills the screen above the taskbar, at 0,0 and with no
		// frame (jwmrc), so Visual-mode coordinates are screen coordinates.
		"--window-position=0,0",
		fmt.Sprintf("--window-size=%d,%d", c.Width, c.Height-panelHeight),
	}
	if smallShm {
		args = append(args, "--disable-dev-shm-usage")
	}
	return args
}

// chromiumEnv is what Chromium's environment adds to the container's.
// Mesa reads both settings in the GPU process, as GL starts (PROJ-165):
// llvmpipe would start one rasteriser thread per CPU, and Mesa's shader
// disk cache a writer thread, and Chromium's GPU sandbox can only start
// in a single-threaded process. With no rasteriser threads llvmpipe draws
// on the GL thread itself: WebGL gets one core instead of all of them,
// which halves a busy WebGL page's frame rate and its CPU (ANTI-BOT.md).
// The disk cache is lost with the container anyway, and the sandbox
// would keep it from opening its files.
func chromiumEnv() []string {
	return []string{"LP_NUM_THREADS=0", "MESA_SHADER_CACHE_DISABLE=true"}
}
