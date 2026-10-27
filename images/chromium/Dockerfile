# syntax=docker/dockerfile:1

# The browser image: ghcr.io/oni-giri/browser-pier-chromium.
# Build from the repo root: docker build -f images/chromium/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY images/chromium/entrypoint ./images/chromium/entrypoint
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/entrypoint ./images/chromium/entrypoint

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium xvfb xdotool \
      gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-plugins-ugly \
      fonts-liberation fonts-dejavu-core fonts-noto-color-emoji fonts-noto-cjk \
      ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

# Fail the build, not the first session, if a plugin or a switch the entrypoint relies on is missing.
RUN for el in ximagesrc videoconvert x264enc fdsink; do gst-inspect-1.0 "$el" >/dev/null || { echo "missing GStreamer element $el"; exit 1; }; done \
 && for sw in remote-debugging-port password-store disable-blink-features use-angle enable-unsafe-swiftshader \
               force-webrtc-ip-handling-policy disable-component-update hide-crash-restore-bubble; do \
      grep -qa -- "$sw" /usr/lib/chromium/chromium || { echo "chromium binary lacks --$sw"; exit 1; }; done

# Chromium runs as pier (uid 1000). /profile is the profile volume and
# /run/browserpier holds the control socket; a fresh named volume mounted on
# either inherits this ownership.
RUN useradd --uid 1000 --user-group --create-home --shell /usr/sbin/nologin pier \
 && mkdir -p /profile /run/browserpier /tmp/.X11-unix \
 && chown pier:pier /profile /run/browserpier \
 && chmod 1777 /tmp/.X11-unix

COPY --from=build /out/entrypoint /usr/local/bin/browserpier-entrypoint

# Changing REBUILD forces a new image without reinstalling packages (used by the persistence test).
ARG REBUILD=0
RUN echo "$REBUILD" > /etc/browserpier-rebuild

USER pier
WORKDIR /home/pier
ENV BROWSERPIER_PROFILE_DIR=/profile \
    BROWSERPIER_CONTROL_SOCKET=/run/browserpier/control.sock
# The entrypoint closes Chromium on SIGTERM; give it time with `docker stop -t`.
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/browserpier-entrypoint"]
