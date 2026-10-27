# syntax=docker/dockerfile:1

# The orchestrator image: ghcr.io/oni-giri/browser-pier.
# The binary is cross-compiled on the build platform, so multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/browserpier ./cmd/browserpier \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/browserpier /usr/local/bin/browserpier
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV BROWSERPIER_DATA_DIR=/data
VOLUME /data
EXPOSE 7437
ENTRYPOINT ["/usr/local/bin/browserpier"]
