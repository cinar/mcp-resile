# syntax=docker/dockerfile:1

# Build stage: compile a static binary (CGO_ENABLED=0, matching Makefile's
# own build target and spec.md §9's "single static binary" packaging goal)
# — required for the distroless/static base below, which carries no libc.
#
# --platform=$BUILDPLATFORM pins this stage to the CI runner's own
# architecture regardless of which platform is being targeted (TARGETARCH,
# set automatically by buildx for a multi-platform build): Go cross-compiles
# natively, so there's no need to emulate an arm64 build stage under QEMU to
# produce an arm64 binary — only the final COPY below needs to land in an
# arm64 image, and that needs no emulation since it never executes anything.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src

ARG TARGETOS
ARG TARGETARCH

# Not derived from .git (not part of the build context — see .dockerignore):
# pass --build-arg VERSION=$(git describe --tags --always --dirty) to stamp
# a real version; defaults to "dev", matching internal/version.Version's own
# default when unset.
ARG VERSION=dev

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/cinar/mcp-resile/internal/version.Version=${VERSION}" \
    -o /out/mcp-resile ./cmd/mcp-resile

# Runtime stage: distroless/static (spec.md §9) — no shell, no package
# manager, nothing beyond the binary itself and CA certificates, which
# egress.Dial needs for an HTTPS backend URL. :nonroot runs as uid/gid
# 65532 rather than root. No --platform here: buildx targets this stage at
# TARGETPLATFORM automatically, pulling the matching distroless variant.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/mcp-resile /mcp-resile

# The gateway's own config.Load default ("mcp-resile.yaml", resolved
# relative to the working directory) isn't meaningful in a container with
# no working-directory convention, so CMD points at a fixed, documented
# path instead: mount your mcp-resile.yaml at /etc/mcp-resile/mcp-resile.yaml
# (read-only), or override CMD with a different --config path.
EXPOSE 8080
ENTRYPOINT ["/mcp-resile"]
CMD ["--config", "/etc/mcp-resile/mcp-resile.yaml"]
