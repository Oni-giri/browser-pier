# syntax=docker/dockerfile:1

# The browser image: ghcr.io/oni-giri/browser-pier-chromium.
# Build from the repo root: docker build -f images/chromium/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY internal/motion ./internal/motion
COPY internal/domainset ./internal/domainset
COPY images/chromium/entrypoint ./images/chromium/entrypoint
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/entrypoint ./images/chromium/entrypoint

# jwm, the window manager and taskbar (jwmrc), depends on a terminal
# emulator, which the desktop never starts. This empty package stands in for
# one, so the image has no terminal for anything to open.
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS no-terminal
RUN mkdir -p /pkg/DEBIAN \
 && printf '%s\n' \
      'Package: browserpier-no-terminal' 'Version: 1' 'Architecture: all' \
      'Maintainer: BrowserPier <https://github.com/Oni-giri/browser-pier>' \
      'Provides: x-terminal-emulator' \
      'Description: stands in for a terminal emulator for jwm' \
      ' The browser container has no terminal; its desktop never starts one.' \
      > /pkg/DEBIAN/control \
 && dpkg-deb --root-owner-group --build /pkg /no-terminal.deb

FROM debian:trixie-slim
COPY --from=no-terminal /no-terminal.deb /tmp/
# The stand-in goes first, so apt doesn't pick a terminal for jwm.
RUN apt-get update \
 && apt-get install -y --no-install-recommends /tmp/no-terminal.deb \
 && apt-get install -y --no-install-recommends \
      chromium chromium-l10n xvfb xdotool xclip jwm \
      gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-plugins-ugly \
      fonts-liberation fonts-dejavu-core fonts-noto-color-emoji fonts-noto-cjk \
      ca-certificates tzdata \
      apparmor \
 && rm -rf /var/lib/apt/lists/* /tmp/no-terminal.deb \
 && for t in x-terminal-emulator xterm urxvt rxvt gnome-terminal konsole; do \
      ! command -v "$t" >/dev/null || { echo "a terminal emulator was installed: $t"; exit 1; }; done

# Fail the build, not the first session, if a plugin or a switch the entrypoint
# relies on is missing. Chromium ignores unknown switches silently, so each
# switch must appear in the binary as a whole NUL-terminated string.
# Chromium's interface languages (chromium-l10n): without them it runs in
# en-US whatever LANGUAGE says, and Intl's locale contradicts
# navigator.languages, which matches the exit location.
RUN for el in ximagesrc videoconvert videoscale x264enc mp4mux fdsink; do gst-inspect-1.0 "$el" >/dev/null || { echo "missing GStreamer element $el"; exit 1; }; done \
 && { command -v xclip >/dev/null || { echo "missing xclip, the live view's clipboard box"; exit 1; }; } \
 && for l in de fr es ja; do test -f /usr/lib/chromium/locales/$l.pak || { echo "missing Chromium locale $l"; exit 1; }; done \
 && tr '\0' '\n' < /usr/lib/chromium/chromium > /tmp/strings \
 && for sw in user-data-dir remote-debugging-port password-store disable-blink-features use-angle enable-unsafe-swiftshader \
               webrtc-ip-handling-policy disable-component-update disable-background-networking no-first-run \
               no-default-browser-check hide-crash-restore-bubble noerrdialogs window-position window-size disable-dev-shm-usage proxy-server host-resolver-rules; do \
      LC_ALL=C grep -qaxF -- "$sw" /tmp/strings || { echo "chromium binary lacks --$sw"; exit 1; }; done \
 && rm /tmp/strings

# Chromium runs as pier (uid 1000) by default. The orchestrator runs it as its
# own uid instead, so it owns the profile files; HOME is set to a path any uid
# can create, since that uid has no home here. /profile is the profile folder
# and /run/browserpier holds the control socket.
RUN useradd --uid 1000 --user-group --create-home --shell /usr/sbin/nologin pier \
 && mkdir -p /profile /run/browserpier /files /tmp/.X11-unix \
 && chown pier:pier /profile /run/browserpier /files \
 && chmod 1777 /tmp/.X11-unix

COPY --from=build /out/entrypoint /usr/local/bin/browserpier-entrypoint
COPY LICENSE NOTICE /usr/share/doc/browserpier/
# Managed policy: hides the "unsupported command-line flag" bar that
# --disable-blink-features would put over every page, and sends downloads
# to /files/downloads without asking, in both modes and without CDP,
# turns off DNS-over-HTTPS, which would go around a profile's proxy, and
# turns off search suggestions, which send what is typed into the address
# bar, an address's path and token included, to the search engine (PROJ-191).
COPY images/chromium/policies.json /etc/chromium/policies/managed/browserpier.json
COPY images/chromium/jwmrc /etc/browserpier/jwmrc
# Fail the build if JWM can't read it: jwm -p exits 0 whatever it finds,
# so any warning counts.
RUN out=$(jwm -p -f /etc/browserpier/jwmrc 2>&1) && test -z "$out" || { echo "jwmrc: $out"; exit 1; }
# BrowserPier's AppArmor profile and its loader, which the compose file's
# apparmor service runs as root (apparmor_parser comes from the apparmor
# package above). Browsers never run it. See images/chromium/apparmor.
COPY images/chromium/apparmor/ /usr/share/browserpier/apparmor/
RUN apparmor_parser --skip-kernel-load --skip-cache /usr/share/browserpier/apparmor/browserpier
# The entrypoint has the sandbox check (`sandbox-check`), with which the
# orchestrator picks the AppArmor profile; older images don't.
LABEL io.browserpier.sandbox-check="1"

# Changing REBUILD forces a new image without reinstalling packages (used by the persistence test).
ARG REBUILD=0
RUN echo "$REBUILD" > /etc/browserpier-rebuild

USER pier
WORKDIR /home/pier
ENV BROWSERPIER_PROFILE_DIR=/profile \
    BROWSERPIER_CONTROL_SOCKET=/run/browserpier/control.sock \
    DISPLAY=:99 \
    HOME=/tmp/home
# The entrypoint closes Chromium on SIGTERM; give it time with `docker stop -t`.
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/browserpier-entrypoint"]
