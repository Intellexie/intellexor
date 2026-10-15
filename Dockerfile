# Intellexor — ACP agent server (ubuntu runtime)
#
# Build:  docker build -t intellexor .
# Run:    docker run -p 8080:8080 -v workspace:/workspace -v intellexor-data:/data intellexor
#         (mount your project at /workspace; override with -e PORT=...)
#
# The container starts Intellexor in ACP mode (Agent Client Protocol): the web UI
# and ACP clients share the same port (0.0.0.0:$PORT).
#
# Runtime toolchain available to the agent: bash, curl, git, node, npm, go,
# playwright (chromium).

# ---- build stage ----
FROM golang:1.26-alpine AS build
ARG GO_VERSION=1.26.2
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/intellexor .

# ---- runtime stage ----
FROM ubuntu:24.04
ARG GO_VERSION=1.26.2
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        bash ca-certificates curl git \
        golang-go \
    && rm -rf /var/lib/apt/lists/*

# Node.js 22 LTS from NodeSource (apt's nodejs is v18 on noble; current
# playwright requires >= 20). Ships its own npm.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x -o /tmp/nodesource.sh && \
    bash /tmp/nodesource.sh && \
    apt-get install -y nodejs && \
    rm -f /tmp/nodesource.sh && \
    node --version && npm --version

# modern Go toolchain (apt's golang-go stays as fallback at /usr/lib/go)
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
        amd64) goarch=amd64 ;; \
        arm64) goarch=arm64 ;; \
        *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" -o /tmp/go.tgz; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm -f /tmp/go.tgz; \
    /usr/local/go/bin/go version

# playwright + chromium (system deps included)
RUN npm install -g playwright && \
    PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright playwright install --with-deps chromium && \
    chmod -R a+rX /opt/ms-playwright && \
    npm cache clean --force

ENV PATH=/usr/local/go/bin:/usr/local/bin:$PATH \
    NODE_PATH=/usr/local/lib/node_modules \
    PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright \
    PORT=8080 \
    ACP_HOST=0.0.0.0 \
    INTELLEXOR_DATA=/data \
    HOME=/data

RUN useradd -m -s /bin/bash intellexor || true && \
    mkdir -p /data /workspace && \
    chown intellexor:intellexor /data 2>/dev/null || true && \
    chmod 777 /data && chmod 755 /workspace

COPY --from=build /out/intellexor /usr/local/bin/intellexor

USER intellexor
WORKDIR /workspace
VOLUME ["/data", "/workspace"]
EXPOSE 4096 8080

ENTRYPOINT ["/usr/local/bin/intellexor", "acp", "--cwd", "/workspace", "--port", "4096"]
CMD []
