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
  sign-in within 5 minutes. Only the chosen method is saved in
  `~/.gemini/antigravity-acp/settings.json`; on macOS the OAuth token itself lives in
  the Keychain (service `gemini`, account `antigravity-acp`), so it cannot be copied
  into a container by mounting `~/.gemini`.
- The sign-in callback is `http://127.0.0.1:<random port>/`, served by the agent. In a
  container your browser cannot reach that port; see "OAuth inside the container".
- The Docker image already contains the Linux build (see below); the simplest
  credential there is `gemini-api-key`.

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

### OAuth inside the container

Antigravity's `oauth-personal` sign-in needs the browser's redirect to reach a small
server the agent opens on a random loopback port. `compose.agy-oauth.yaml` makes that work:

```sh
docker compose -f compose.yaml -f compose.agy-oauth.yaml up -d --build
scripts/agy-login.sh
```

`agy-login.sh` triggers a run, prints (and opens) the Google link, and waits for the
sign-in to finish. Nothing needs to be pasted back:

- the overlay shrinks the container's ephemeral port range to `38000-38031`, so the
  agent's callback port, and therefore the `redirect_uri` Google sees, falls in it;
- that range is published on the host's loopback (`127.0.0.1:38000-38031`);
- the agent binds only `127.0.0.1`, which Docker cannot forward to, so the bridge
  (`-oauth-relay 38000-38031`) relays the one port the agent is listening on to the
  container address.

The relay is deliberately narrow. It is only open while the `authenticate` call is in
flight, each port is relayed for one connection, and it binds a port only while the
agent is listening on it. After sign-in the agent opens other loopback services (a
local credentials-injection proxy among them); an always-on relay would have exposed
those to the compose network and the published ports. It also must not hold the whole
range: the kernel takes outgoing connections' source ports from the same narrow range
and skips any port something has bound, so holding all 32 made every outgoing
connection fail with `EADDRNOTAVAIL`.

The sign-in is stored as `acp_token.json` and `settings.json` under
`/home/node/.gemini/antigravity-acp/` (on a Linux container, not the macOS Keychain);
the overlay keeps that directory on the `agy-home` volume, so it survives restarts and
recreation. `docker volume rm <project>_agy-home` signs out.

Verified end to end in Docker: Google sign-in through the published port, the agent
answering `pong` through the bridge, the login surviving a container restart without a
new sign-in, and the agent's other loopback listeners being unreachable from the host.

Notes:

- The image installs `ca-certificates`. Without it Antigravity's Python server fails
  with `CERTIFICATE_VERIFY_FAILED` and every model call ends as "The model is currently
  unreachable" (502), so a run never finishes even though sign-in worked.
- The 32-port range also bounds concurrent outgoing connections to one destination.
  Widen the range and the published ports together if you see `EADDRNOTAVAIL`.
- Without the overlay, `scripts/agy-login.sh` falls back to asking you to paste the
  failed `http://127.0.0.1:<port>/?code=...` URL, which it replays inside the container.
- `agy-login.sh` needs `ACP_AGENT=antigravity` and `ACP_AUTH_METHOD=oauth-personal`
  in the container (the overlay sets both).

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
