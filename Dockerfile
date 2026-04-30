# =============================================================================
# demo-parser — standalone, versioned image.
#
# Wraps markus-wa/demoinfocs-golang. Publishes a tiny static binary at
# /usr/local/bin/demo-parser and treats that path as the contract.
# Consumed by api/Dockerfile via:
#
#   ARG DEMO_PARSER_IMAGE=ghcr.io/5stackgg/demo-parser:latest
#   FROM ${DEMO_PARSER_IMAGE} AS demo-parser
#   ...
#   COPY --from=demo-parser /usr/local/bin/demo-parser /usr/local/bin/demo-parser
#
# Same pattern as game-streamer/openhud — keeps the parser versioned
# independently of the api so we don't have to vendor Go source into
# the api repo / build context.
#
# Build & publish:
#   ./push-latest.sh                  # tags :latest
#   DEMO_PARSER_REF=v0.1.0 ./push-latest.sh
#
# Pin in api with:
#   docker build --build-arg DEMO_PARSER_IMAGE=ghcr.io/5stackgg/demo-parser:v0.1.0 .
#
# Standalone HTTP mode (also still works for ad-hoc testing):
#   docker run --rm -p 8080:8080 ghcr.io/5stackgg/demo-parser server
#
# CLI mode (what the api uses):
#   cat demo.dem | docker run --rm -i ghcr.io/5stackgg/demo-parser parse
# =============================================================================

# syntax=docker/dockerfile:1.7

# ---- Stage 1: build the Go binary -----------------------------------------
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# CGO disabled: produces a fully static binary that runs on alpine /
# scratch / debian / busybox / wherever the consumer wants to drop it.
# -trimpath strips local file paths from the binary; -s -w strip
# debug + symbol tables (~30% size savings, no runtime impact).
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/demo-parser \
      ./cmd/server

# ---- Stage 2: minimal runtime + ship the binary at a stable path ----------
# distroless/static is just libc + tzdata + ca-certificates; no shell,
# no package manager. The binary is the entire user-space. ~25MB image,
# minimal attack surface.
FROM gcr.io/distroless/static-debian12

# /usr/local/bin/demo-parser is the contract: api/Dockerfile copies
# from this exact path. Don't change it without coordinating both
# build pipelines.
COPY --from=build /out/demo-parser /usr/local/bin/demo-parser

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/demo-parser"]
# Default to `server` (HTTP) so `docker run` without args still does
# something useful for testing. The api invokes `parse` explicitly.
CMD ["server"]
