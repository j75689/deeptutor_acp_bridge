FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/deeptutor_acp_bridge ./cmd/deeptutor_acp_bridge

# Antigravity's ACP server, as published in the ACP Registry (antigravity-acp).
# Versions and hashes come from its agent.json; bump all three together.
FROM debian:bookworm-slim AS agy
ARG TARGETARCH
ARG AGY_VERSION=1.4.3
ARG AGY_SHA256_AMD64=0634ce5306ba92c932945455e7a83fda78e2d99c33fa8aa6ee0566206a382714
ARG AGY_SHA256_ARM64=05c153443b0b1175151c760eeddea538950bc9843e317d54786a1f03670182bc
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip \
 && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) file=linux-x86_64; sha="$AGY_SHA256_AMD64" ;; \
      arm64) file=linux-arm64;  sha="$AGY_SHA256_ARM64" ;; \
      *) echo "unsupported arch: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/agy.zip "https://dl.google.com/agy-extensions/releases/linux/agy-acp-server-${AGY_VERSION}-${file}.zip"; \
    echo "${sha}  /tmp/agy.zip" | sha256sum -c -; \
    mkdir -p /opt/agy && unzip -q /tmp/agy.zip -d /opt/agy

# The Claude adapter is a Node program, so the runtime image needs Node.
FROM node:22-slim
ARG ADAPTER_VERSION=0.95.0
# Node bundles its own CA list, but Antigravity's Python server uses the system store
# and fails with CERTIFICATE_VERIFY_FAILED ("model is currently unreachable") without it.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @agentclientprotocol/claude-agent-acp@${ADAPTER_VERSION} \
 && npm cache clean --force \
 && mkdir -p /workspace /home/node/.gemini \
 && chown node:node /workspace /home/node/.gemini
COPY --from=build /out/deeptutor_acp_bridge /usr/local/bin/deeptutor_acp_bridge
COPY --from=agy /opt/agy /opt/agy
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
USER node
WORKDIR /workspace
EXPOSE 8642
ENTRYPOINT ["docker-entrypoint.sh"]
