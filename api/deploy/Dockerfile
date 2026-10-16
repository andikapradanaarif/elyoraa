# Build stage — keep in sync with the `go` directive in api/go.mod.
FROM golang:1.25-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /build/elyoraa ./cmd/server && \
    CGO_ENABLED=0 go build -o /build/elyoraa-migrate ./cmd/migrate

# Runtime stage
# Use Debian slim because the host-provided `devin` binary is usually glibc-linked.
FROM debian:trixie-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends git ca-certificates bubblewrap socat curl && \
    rm -rf /var/lib/apt/lists/*

# Devin CLI, bundled so no host bind-mount is required. The release manifest
# at static.devin.ai/cli/<version>/manifest.json carries per-platform tarball
# URLs + sha256 — verify the checksum, then lay the tarball down at
# /usr/local (bin/devin + share/devin/docs land where the CLI expects them).
# Pin a release with: docker build --build-arg DEVIN_CLI_VERSION=3000.11.92 ...
ARG DEVIN_CLI_VERSION=current
RUN set -eu; \
    case "$(uname -m)" in \
      x86_64)  target="x86_64-unknown-linux" ;; \
      aarch64) target="aarch64-unknown-linux" ;; \
      *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;; \
    esac; \
    manifest=$(curl -fsSL "https://static.devin.ai/cli/${DEVIN_CLI_VERSION}/manifest.json"); \
    entry=$(printf '%s' "$manifest" | grep -o "\"${target}\"[[:space:]]*:[[:space:]]*{[^}]*}"); \
    url=$(printf '%s' "$entry" | grep -o '"url"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/'); \
    sha=$(printf '%s' "$entry" | grep -o '"sha256"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/'); \
    [ -n "$url" ] && [ -n "$sha" ] || { echo "no ${target} bundle in manifest" >&2; exit 1; }; \
    curl -fsSL "$url" -o /tmp/devin.tar.gz; \
    echo "${sha}  /tmp/devin.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/devin.tar.gz -C /usr/local bin/devin share; \
    rm -f /tmp/devin.tar.gz

RUN useradd -m -s /bin/sh elyoraa

WORKDIR /app
COPY --from=builder /build/elyoraa /app/elyoraa
COPY --from=builder /build/elyoraa-migrate /app/elyoraa-migrate
COPY --from=builder /build/prompts /app/prompts
COPY deploy/git-askpass.sh /app/git-askpass.sh
COPY deploy/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/git-askpass.sh /app/entrypoint.sh

# Devin skill: install to the user's global skill directory.
RUN mkdir -p /home/elyoraa/.config/devin/skills/pr-review
COPY deploy/skills/pr-review/SKILL.md /home/elyoraa/.config/devin/skills/pr-review/SKILL.md
RUN chown -R elyoraa:elyoraa /home/elyoraa

# Pre-create workspace, token, devin-config, queue, and tmp dirs.
RUN mkdir -p /var/lib/elyoraa/workspace /var/lib/elyoraa/tokens /var/lib/elyoraa/devin-config /var/lib/elyoraa/queue /tmp/elyoraa && \
    chown -R elyoraa:elyoraa /var/lib/elyoraa /tmp/elyoraa

USER elyoraa
ENV HOME=/home/elyoraa
ENV XDG_CONFIG_HOME=/home/elyoraa/.config

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD sh -c 'PORT="${PORT:-8080}"; [ -f /app/.env ] && . /app/.env 2>/dev/null; curl -f "http://localhost:${PORT:-8080}/health"' || exit 1
ENTRYPOINT ["/app/entrypoint.sh"]
