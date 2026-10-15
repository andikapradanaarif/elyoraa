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

# Devin CLI must be installed in the image or mounted in. This expects `devin` in PATH.
# If Devin is distributed as a tarball, add the installation step here.

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
