package main

// Visual mode's accessibility (PROJ-150). Chromium publishes its
// accessibility tree through the AT-SPI bridge (libatk-bridge, which the
// chromium package already pulls in), and the tree reader (tree.go) reads
// the page from it, so Visual mode reads the page without CDP.
//
// The tree goes over the session bus (sessionbus.go), the one private
// dbus-daemon the entrypoint runs for the file chooser portal: there is no
// second bus, no system bus, and no at-spi2-core launcher or registry.
// Chromium finds the bus through AT_SPI_BUS_ADDRESS, and
// ACCESSIBILITY_ENABLED=1 makes it load the bridge with no launcher to
// ask (see images/chromium/ANTI-BOT.md). The bus's rules hold for the
// tree too: a path socket in a 0700 folder of the browser's uid, EXTERNAL
// auth, no TCP or abstract socket, no activation, and no well-known name
// but the portal's. The tree reader calls only the read methods on its
// allow-list (tree.go). API mode sets neither variable nor Chromium's
// accessibility switch, so the bus carries no tree there.

// a11yEnv is the environment that points Chromium's accessibility at the
// bus at addr.
func a11yEnv(addr string) []string {
	return []string{"AT_SPI_BUS_ADDRESS=" + addr, "ACCESSIBILITY_ENABLED=1"}
}
