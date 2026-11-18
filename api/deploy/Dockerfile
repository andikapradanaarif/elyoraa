# Build stage — keep in sync with the `go` directive in api/go.mod.
FROM golang:1.25-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /build/elyoraa ./cmd/server && \
    CGO_ENABLED=0 go build -o /build/elyoraa-migrate ./cmd/migrate

# Build the user's Crush fork, pinned to the installed system revision.
# The fork retains the upstream module path, so build from a checkout.
FROM golang:1.27.4 AS crush-builder
ARG CRUSH_REPO=https://github.com/andikapradanaarif/crush.git
ARG CRUSH_REF=759066acc9c0fd62dcb74bc8473e171dee9c11d2
RUN git init /crush && git -C /crush remote add origin "$CRUSH_REPO" && \
    git -C /crush fetch --depth 1 origin "$CRUSH_REF" && \
    git -C /crush checkout --detach FETCH_HEAD
WORKDIR /crush
RUN CGO_ENABLED=0 go build -o /out/crush .

FROM debian:trixie-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends git ca-certificates curl && \
    rm -rf /var/lib/apt/lists/*
COPY --from=crush-builder /out/crush /usr/local/bin/crush

RUN useradd -m -s /bin/sh elyoraa

WORKDIR /app
COPY --from=builder /build/elyoraa /app/elyoraa
COPY --from=builder /build/elyoraa-migrate /app/elyoraa-migrate
COPY --from=builder /build/prompts /app/prompts
COPY deploy/git-askpass.sh /app/git-askpass.sh
COPY deploy/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/git-askpass.sh /app/entrypoint.sh

# Crush reads the user's trusted config/auth data; job state is temporary.
RUN mkdir -p /home/elyoraa/.config/crush /home/elyoraa/.local/share/crush && \
    chown -R elyoraa:elyoraa /home/elyoraa

# Pre-create workspace, token, queue, and tmp dirs.
RUN mkdir -p /var/lib/elyoraa/workspace /var/lib/elyoraa/tokens /var/lib/elyoraa/queue /tmp/elyoraa && \
    chown -R elyoraa:elyoraa /var/lib/elyoraa /tmp/elyoraa

USER elyoraa
ENV HOME=/home/elyoraa
ENV XDG_CONFIG_HOME=/home/elyoraa/.config

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD sh -c 'PORT="${PORT:-8080}"; [ -f /app/.env ] && . /app/.env 2>/dev/null; curl -f "http://localhost:${PORT:-8080}/health"' || exit 1
ENTRYPOINT ["/app/entrypoint.sh"]
