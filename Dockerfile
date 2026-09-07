# syntax=docker/dockerfile:1

# Build stage: compile a static binary (CGO_ENABLED=0, matching Makefile's
# own build target and spec.md §9's "single static binary" packaging goal)
# — required for the distroless/static base below, which carries no libc.
FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mcp-resile ./cmd/mcp-resile

# Runtime stage: distroless/static (spec.md §9) — no shell, no package
# manager, nothing beyond the binary itself and CA certificates, which
# egress.Dial needs for an HTTPS backend URL. :nonroot runs as uid/gid
# 65532 rather than root.
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
