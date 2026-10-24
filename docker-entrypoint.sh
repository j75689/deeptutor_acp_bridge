#!/bin/sh
# Picks the ACP agent from ACP_AGENT (claude | antigravity), or runs the command
# given as container arguments. ACP_AUTH_METHOD overrides the auth method.
set -eu

if [ "$#" -eq 0 ]; then
  case "${ACP_AGENT:-claude}" in
    claude)
      set -- claude-agent-acp
      ;;
    antigravity)
      set -- /opt/agy/agy_acp_server.par --uid=
      : "${ACP_AUTH_METHOD:=gemini-api-key}"
      ;;
    *)
      echo "unknown ACP_AGENT '${ACP_AGENT}' (use claude or antigravity, or pass a command)" >&2
      exit 2
      ;;
  esac
fi

if [ -n "${ACP_AUTH_METHOD:-}" ]; then
  exec deeptutor_acp_bridge -listen 0.0.0.0:8642 -cwd /workspace -auth-method "$ACP_AUTH_METHOD" -- "$@"
fi
exec deeptutor_acp_bridge -listen 0.0.0.0:8642 -cwd /workspace -- "$@"
