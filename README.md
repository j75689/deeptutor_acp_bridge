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

### Antigravity

Google publishes the ACP server as `agy_acp_server` in the
[ACP Registry](https://github.com/agentclientprotocol/registry/tree/main/antigravity-acp)
(entry `antigravity-acp`). Download the zip for your platform from that entry's
`agent.json`, check its `sha256`, and unzip it; keep `localharness_external` next to
`agy_acp_server.par`.

```sh
bin/deeptutor_acp_bridge -listen 127.0.0.1:8642 -cwd /path/to/workdir \
  -auth-method oauth-personal -- /path/to/agy_acp_server.par
```

The agent refuses to create a session until it is authenticated. `-auth-method`
makes the bridge call ACP `authenticate` with that method id after `initialize`;
Antigravity accepts `oauth-personal`, `gemini-api-key` (needs `GEMINI_API_KEY`),
`agent-platform` and others (its error message lists them).

- `oauth-personal` prints a Google sign-in link and opens a browser; finish the
  sign-in within 5 minutes. The login and the chosen method are saved under
  `~/.gemini/antigravity-acp/`, and the sign-in callback is `http://127.0.0.1:<port>`
  on the same machine, so do it on the host rather than inside a container.
- The Docker image already contains the Linux build (see below); there use
  `gemini-api-key`, since the browser sign-in callback cannot reach a container.

## Docker Compose (DeepTutor + bridge)

`compose.yaml` starts the official DeepTutor image and this bridge (running the
Claude adapter) on one network. The bridge is not published to the host.

```sh
cp .env.example .env
# fill in BRIDGE_API_KEY (openssl rand -hex 24) and a Claude credential:
#   CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`) or ANTHROPIC_API_KEY
docker compose up -d
```

Choose the agent with `ACP_AGENT` in `.env`:

| `ACP_AGENT` | Agent | Credential |
|---|---|---|
| `claude` (default) | `claude-agent-acp` | `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` |
| `antigravity` | `agy_acp_server` (ACP Registry build, hash-checked at image build) | `GEMINI_API_KEY` (auth method `gemini-api-key`; override with `ACP_AUTH_METHOD`) |

DeepTutor can point at one bridge address, so the container drives one agent at a
time. To run something else, pass a command to the `acp-bridge` service
(`command: ["my-acp-agent", "--flag"]`), optionally with `ACP_AUTH_METHOD`.

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

Switching agent only changes `ACP_AGENT` in `.env`; the DeepTutor connection stays the same.

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
