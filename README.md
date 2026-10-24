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
