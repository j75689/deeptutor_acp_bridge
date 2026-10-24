FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/deeptutor_acp_bridge ./cmd/deeptutor_acp_bridge

# The ACP adapter is a Node program, so the runtime image needs Node.
FROM node:22-slim
ARG ADAPTER_VERSION=0.95.0
RUN npm install -g @agentclientprotocol/claude-agent-acp@${ADAPTER_VERSION} \
 && npm cache clean --force \
 && mkdir -p /workspace && chown node:node /workspace
COPY --from=build /out/deeptutor_acp_bridge /usr/local/bin/deeptutor_acp_bridge
USER node
WORKDIR /workspace
EXPOSE 8642
# The agent command is the CMD, so another ACP agent can be swapped in.
ENTRYPOINT ["deeptutor_acp_bridge", "-listen", "0.0.0.0:8642", "-cwd", "/workspace", "--"]
CMD ["claude-agent-acp"]
