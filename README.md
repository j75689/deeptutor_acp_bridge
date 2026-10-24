# deeptutor_acp_bridge

Lets DeepTutor's **Hermes Agent (remote)** backend drive any
[ACP](https://agentclientprotocol.com) agent. The bridge speaks the Hermes
gateway HTTP/SSE contract to DeepTutor and ACP (JSON-RPC over stdio) to the agent.

```
DeepTutor hermes_remote ──HTTP+SSE──▶ deeptutor_acp_bridge ──stdio ACP──▶ agent
```

## Run

```sh
go build -o bin/deeptutor_acp_bridge ./cmd/deeptutor_acp_bridge

export DEEPTUTOR_ACP_BRIDGE_KEY=change-me
bin/deeptutor_acp_bridge -listen 127.0.0.1:8642 -cwd /path/to/workdir -- <acp-agent> [args...]
```

In DeepTutor, configure the Hermes Agent (remote) connection with
`base_url = http://127.0.0.1:8642` and an API-key env var holding the same token.

### Claude

Uses the ACP adapter for Claude Code
([`@agentclientprotocol/claude-agent-acp`](https://www.npmjs.com/package/@agentclientprotocol/claude-agent-acp);
the old `@zed-industries/*` names are deprecated). It reuses the login of the
local `claude` CLI, so the bridge needs no Anthropic key.

```sh
export DEEPTUTOR_ACP_BRIDGE_KEY=change-me
bin/deeptutor_acp_bridge -listen 127.0.0.1:8642 -cwd /path/to/workdir \
  -- npx -y @agentclientprotocol/claude-agent-acp
```

- The first call takes a few seconds (npx download + SDK start-up). To avoid it,
  `npm i -g @agentclientprotocol/claude-agent-acp` and pass `claude-agent-acp` instead of `npx -y ...`.
- Claude asks permission before writing files or running commands. Turn on
  **auto approve** for the connection in DeepTutor, otherwise every request is
  denied and Claude cannot act.
- Tool titles can change mid-call (e.g. "Preparing file…" → "Write hello.txt").

## Docker Compose (DeepTutor + bridge)

`compose.yaml` starts the official DeepTutor image and this bridge (running the
Claude adapter) on one network. The bridge is not published to the host.

```sh
cp .env.example .env
# fill in BRIDGE_API_KEY (openssl rand -hex 24) and a Claude credential:
#   CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`) or ANTHROPIC_API_KEY
docker compose up -d
```

DeepTutor is then at <http://localhost:3782>. A login stored in the macOS/Windows
keychain is not visible inside a container, which is why a token or API key is needed.

### Connect DeepTutor

Once, in DeepTutor's Connected Agents settings, add **Hermes Agent (remote)** with
`base_url = http://acp-bridge:8642` and `api_key_env = DEEPTUTOR_HERMES_REMOTE_API_KEY`
(compose already sets that variable to `BRIDGE_API_KEY`). Keep **auto approve** on.

Or through the API, which writes `data/user/settings/subagent.json`:

```sh
curl -X PUT http://localhost:8001/api/subagents/settings \
  -H 'Content-Type: application/json' \
  -d '{"backends":{"hermes_remote":{"enabled":true,"base_url":"http://acp-bridge:8642","api_key_env":"DEEPTUTOR_HERMES_REMOTE_API_KEY","auto_approve":true}}}'
```

To use another ACP agent, change the container command, e.g.
`command: ["gemini", "--experimental-acp"]` on `acp-bridge` (the image then needs that CLI installed).

## Endpoints

| Endpoint | ACP mapping |
|---|---|
| `GET /v1/capabilities` | advertises the five required features |
| `POST /v1/runs` | `session/new` (once per session) + `session/prompt` |
| `GET /v1/runs/{id}/events` | `session/update` → `message.delta`, `reasoning.available`, `tool.started`, `tool.completed`; `session/request_permission` → `approval.request` |
| `POST /v1/runs/{id}/approval` | answers the pending permission request (`once` / `deny`) |
| `POST /v1/runs/{id}/stop` | `session/cancel` |
| `GET /api/sessions/{id}/messages` | bridge-kept user/assistant history |

## Behaviour notes

- One agent process is shared by all sessions and restarted if it dies; a session
  whose ACP session was lost gets its history replayed as context.
- ACP has no system prompt, so DeepTutor's `instructions` are prepended to the
  first prompt of each ACP session. The request `model` is ignored.
- Sessions and history are in memory only; a bridge restart makes DeepTutor start fresh.
- The bridge advertises no fs/terminal client capabilities; the agent uses its own tools.

## Test

```sh
go test -race ./...
```
