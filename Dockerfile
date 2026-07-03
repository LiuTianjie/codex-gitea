# syntax=docker/dockerfile:1

# ---------- console frontend ----------
FROM node:22-bookworm-slim AS console-ui
WORKDIR /src/internal/console/frontend
COPY internal/console/frontend/package*.json ./
RUN npm ci
COPY internal/console/frontend ./
RUN npm run build

# ---------- build stage ----------
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=console-ui /src/internal/console/static/dist ./internal/console/static/dist
# Static build so it runs on the slim runtime without libc surprises.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/codex-gitea ./cmd/codex-gitea

# ---------- runtime stage ----------
FROM debian:bookworm-slim AS runtime
ARG CC_SWITCH_VERSION=5.8.6

# git: gitcache mirrors/worktrees. node: runs the codex CLI. ca-certs+curl: TLS + healthcheck.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates curl gnupg gosu \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    # Pin the codex CLI. NOTE: spiked behavior (thread.started event id,
    # generic `codex exec` honoring --output-schema, resume) verified on 0.133.0;
    # re-verify if you bump this.
    && npm install -g @openai/codex@0.135.0 @anthropic-ai/claude-code \
    && arch="$(dpkg --print-architecture)" \
    && case "$arch" in \
        amd64) cc_arch="linux-x64-musl"; cc_sha256="6b6d7d163d64355e5d0d047244fa6a7538b54cc5017d9c2eb97bf9babd339978" ;; \
        arm64) cc_arch="linux-arm64-musl"; cc_sha256="c2a26abb12314bf60e56c885fdb71dfe13f4b548aa5497526950ac3b5d02bfd1" ;; \
        *) echo "unsupported arch for cc-switch: $arch" >&2; exit 1 ;; \
      esac \
    && curl -fsSL -o /tmp/cc-switch.tar.gz "https://github.com/SaladDay/cc-switch-cli/releases/download/v${CC_SWITCH_VERSION}/cc-switch-cli-${cc_arch}.tar.gz" \
    && echo "${cc_sha256}  /tmp/cc-switch.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/cc-switch.tar.gz -C /tmp \
    && install -m 0755 /tmp/cc-switch /usr/local/bin/cc-switch \
    && rm -f /tmp/cc-switch.tar.gz /tmp/cc-switch \
    && apt-get purge -y gnupg && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

# Non-root user; volumes are chowned to it in the entrypoint.
RUN useradd --create-home --uid 10001 app

COPY --from=build /out/codex-gitea /usr/local/bin/codex-gitea
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENV LISTEN_ADDR=:8080 \
    DB_PATH=/data/codex-gitea.db \
    CACHE_DIR=/cache \
    WORK_DIR=/work \
    CODEX_HOME=/codex-home \
    CODEX_AUTH_MODE=ccswitch \
    CLAUDE_HOME=/claude-home \
    CC_SWITCH_CONFIG_DIR=/cc-switch

VOLUME ["/data", "/cache", "/work", "/codex-home", "/claude-home", "/cc-switch"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["codex-gitea"]
