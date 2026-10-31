package main

import "fmt"

// chromiumArgs is Chromium's command line. It never includes
// --enable-automation, --headless or --no-sandbox, and spoofs nothing.
// smallShm is set when /dev/shm is too small for Chromium's shared memory;
// proxied when the profile has a proxy, served by the forwarder.
func chromiumArgs(c config, smallShm, proxied bool) []string {
	args := []string{
		"--user-data-dir=" + c.ProfileDir,
		// Chromium binds this to the container's loopback; the control socket relays it.
		"--remote-debugging-port=9222",
		// A fixed key, so cookies decrypt in every container and survive image updates.
		"--password-store=basic",
		"--disable-blink-features=AutomationControlled",
		// No GPU in the container: WebGL runs on SwiftShader.
		"--use-angle=swiftshader",
		"--enable-unsafe-swiftshader",
		// A proxied profile can't leak the host IP over WebRTC.
		"--webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--disable-component-update",
		"--disable-background-networking",
		"--no-first-run",
		"--no-default-browser-check",
		"--hide-crash-restore-bubble",
		"--noerrdialogs",
		// There is no window manager, so place the window over the whole display:
		// Visual-mode coordinates are screen coordinates.
		"--window-position=0,0",
		fmt.Sprintf("--window-size=%d,%d", c.Width, c.Height),
	}
	if smallShm {
		args = append(args, "--disable-dev-shm-usage")
	}
	if proxied {
		args = append(args,
			// Everything goes through the forwarder; with a single proxy
			// and no DIRECT fallback, if it's down nothing goes out.
			"--proxy-server=socks5://"+forwarderAddr,
			// And nothing resolves a name locally, so DNS can't leak
			// around the proxy either.
			"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		)
	}
	return args
}
