#!/bin/bash
# Picks the ACP agent from ACP_AGENT (claude | antigravity), or runs the command
# given as container arguments.
#
#   ACP_AUTH_METHOD  ACP auth method id to authenticate with (antigravity: gemini-api-key)
#   ACP_OAUTH_RELAY  port range (lo-hi) relayed to the agent's loopback so a browser
#                    sign-in callback can reach it (see compose.agy-oauth.yaml)
set -euo pipefail

agent=("$@")
if [ "${#agent[@]}" -eq 0 ]; then
  case "${ACP_AGENT:-claude}" in
    claude)
      agent=(claude-agent-acp)
      ;;
    antigravity)
      agent=(/opt/agy/agy_acp_server.par --uid=)
      : "${ACP_AUTH_METHOD:=gemini-api-key}"
      ;;
    *)
      echo "unknown ACP_AGENT '${ACP_AGENT}' (use claude or antigravity, or pass a command)" >&2
      exit 2
      ;;
  esac
fi

flags=(-listen 0.0.0.0:8642 -cwd /workspace)
[ -n "${ACP_AUTH_METHOD:-}" ] && flags+=(-auth-method "$ACP_AUTH_METHOD")
[ -n "${ACP_OAUTH_RELAY:-}" ] && flags+=(-oauth-relay "$ACP_OAUTH_RELAY")
exec deeptutor_acp_bridge "${flags[@]}" -- "${agent[@]}"
