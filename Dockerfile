# syntax=docker/dockerfile:1

# The orchestrator image: ghcr.io/oni-giri/browser-pier.

# The docs site (site/), built once on the build platform: the binary
# embeds it (internal/docs), so each version serves its own docs.
FROM --platform=$BUILDPLATFORM node:24.22.2-alpine AS docs
WORKDIR /src/site
COPY site/package.json site/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY site/ ./
# The theme uses the web UI's fonts and grain.
COPY internal/ui/static/ /src/internal/ui/static/
RUN npm run build

# The binary is cross-compiled on the build platform, so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
COPY --from=docs /src/site/dist/ ./internal/docs/site/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/browserpier ./cmd/browserpier \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/browserpier /usr/local/bin/browserpier
COPY LICENSE NOTICE /usr/share/doc/browserpier/
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV BROWSERPIER_DATA_DIR=/data
VOLUME /data
# 7437 the app; 7438 live views (WebRTC), UDP with TCP as the fallback
EXPOSE 7437/tcp 7438/tcp 7438/udp
ENTRYPOINT ["/usr/local/bin/browserpier"]
