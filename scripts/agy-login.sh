#!/usr/bin/env bash
# Sign Antigravity in with Google OAuth (oauth-personal) inside the acp-bridge
# container, where the browser cannot reach the agent's callback port itself.
#
#   scripts/agy-login.sh            # interactive
#   scripts/agy-login.sh --url 'http://127.0.0.1:PORT/?code=...&state=...'
#
# Needs the service running with ACP_AGENT=antigravity and ACP_AUTH_METHOD=oauth-personal.
# Easiest: docker compose -f compose.yaml -f compose.agy-oauth.yaml up -d --build
# (that overlay also publishes the callback port range, so no URL has to be pasted).
set -euo pipefail

SERVICE=${ACP_BRIDGE_SERVICE:-acp-bridge}
WAIT_LINK=${WAIT_LINK_SECONDS:-90}
WAIT_DONE=${WAIT_DONE_SECONDS:-120}
callback_url=""

usage() {
  sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --url) callback_url=${2:?--url needs a value}; shift 2 ;;
    --service) SERVICE=${2:?--service needs a value}; shift 2 ;;
    -h|--help) usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 2 ;;
  esac
done

cd "$(dirname "$0")/.."
dc() { docker compose "$@"; }
exec_in() { dc exec -T "$SERVICE" "$@"; }
die() { echo "error: $*" >&2; exit 1; }

# node one-liners run inside the container (node is already there for the adapter).
NODE_START='(async()=>{const r=await fetch("http://127.0.0.1:8642/v1/runs",{method:"POST",headers:{Authorization:"Bearer "+process.env.DEEPTUTOR_ACP_BRIDGE_KEY},body:JSON.stringify({input:"Reply with the single word: ready"})});const j=await r.json();console.log(j.run_id||"")})()'
NODE_STATE='(async()=>{const ac=new AbortController();setTimeout(()=>ac.abort(),2500);let buf="";try{const r=await fetch("http://127.0.0.1:8642/v1/runs/"+process.argv[1]+"/events",{headers:{Authorization:"Bearer "+process.env.DEEPTUTOR_ACP_BRIDGE_KEY},signal:ac.signal});for await(const c of r.body)buf+=Buffer.from(c).toString()}catch(e){}const m=buf.match(/event: (run\.(?:completed|failed|cancelled))\ndata: (.*)/);console.log(m?m[1]+" "+m[2]:"pending")})()'
NODE_RELAY='(async()=>{const r=await fetch(process.argv[1],{redirect:"manual"});console.log(r.status)})().catch(e=>{console.log("error "+e.message);process.exit(1)})'

[ -n "$(dc ps -q "$SERVICE")" ] || die "service '$SERVICE' is not running (docker compose up -d $SERVICE)"

agent=$(exec_in sh -c 'printf %s "${ACP_AGENT:-claude}"')
method=$(exec_in sh -c 'printf %s "${ACP_AUTH_METHOD:-}"')
[ "$agent" = antigravity ] || die "ACP_AGENT is '$agent', expected antigravity"
[ "$method" = oauth-personal ] || die "ACP_AUTH_METHOD is '${method:-unset}', expected oauth-personal"

exec_in touch /tmp/.agy-login-marker

echo "Starting a run to trigger sign-in..."
run_id=$(exec_in node -e "$NODE_START" | tr -d '\r\n')
[ -n "$run_id" ] || die "bridge did not accept a run (is BRIDGE_API_KEY set in the container?)"

state() { exec_in node -e "$NODE_STATE" "$run_id" | tr -d '\r'; }
report_terminal() {
  case "$1" in
    run.completed*) return 0 ;;
    run.failed*|run.cancelled*) echo "run ended: $1" >&2; return 1 ;;
  esac
  return 2
}

link=""
deadline=$((SECONDS + WAIT_LINK))
while [ "$SECONDS" -lt "$deadline" ]; do
  s=$(state)
  if report_terminal "$s"; then
    echo "Already signed in: the run completed without asking for a login."
    exit 0
  elif [ $? -eq 1 ]; then
    exit 1
  fi
  # Still pending, so the agent is waiting for sign-in. Take the newest link in the
  # logs: an earlier attempt may still be waiting and will not print another.
  link=$(dc logs --no-log-prefix "$SERVICE" 2>&1 \
    | grep -o 'https://accounts\.google\.com/o/oauth2/[^[:space:]]*' | tail -1 || true)
  [ -n "$link" ] && break
  sleep 2
done
[ -n "$link" ] || die "no sign-in link appeared in the logs within ${WAIT_LINK}s (docker compose logs $SERVICE)"

echo
echo "1. Open this link and sign in with Google:"
echo
echo "   $link"
echo
if [ -z "${NO_OPEN:-}" ]; then
  if command -v open >/dev/null 2>&1; then open "$link" >/dev/null 2>&1 || true
  elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$link" >/dev/null 2>&1 || true; fi
fi

relay=$(exec_in sh -c 'printf %s "${ACP_OAUTH_RELAY:-}"')
if [ -n "$relay" ] && [ -z "$callback_url" ]; then
  # compose.agy-oauth.yaml: the callback port is published and relayed by the bridge,
  # so the browser's redirect reaches the agent without any help.
  echo "2. Sign in; the browser's redirect reaches the agent on its own (relay $relay)."
else
  if [ -z "$callback_url" ]; then
    echo "2. The browser then lands on a http://127.0.0.1:<port>/?code=... page that fails to"
    echo "   load. Copy the full URL from the address bar and paste it here."
    echo "   (Use compose.agy-oauth.yaml to skip this step.)"
    printf "   URL: "
    read -r callback_url || true
  fi
  case "$callback_url" in
    http://127.0.0.1:[0-9]*/*|http://localhost:[0-9]*/*) ;;
    *) die "that is not a http://127.0.0.1:<port>/... callback URL" ;;
  esac
  echo "Relaying the callback into the container..."
  status=$(exec_in node -e "$NODE_RELAY" "$callback_url" | tr -d '\r') || die "relay failed: $status"
  echo "agent answered: HTTP $status"
fi

echo "Waiting for the run to finish..."
deadline=$((SECONDS + WAIT_DONE))
final=pending
while [ "$SECONDS" -lt "$deadline" ]; do
  final=$(state)
  [ "$final" != pending ] && break
  sleep 2
done

if report_terminal "$final"; then
  echo "Signed in. The run completed."
else
  [ "$final" = pending ] && echo "The run is still pending after ${WAIT_DONE}s." >&2
  exit 1
fi

echo
echo "Files written under /home/node during sign-in (to see what to persist):"
exec_in find /home/node -type f -newer /tmp/.agy-login-marker \
  -not -path '*/conversations/*' -not -path '*/brain/*' -not -path '*/.npm/*' 2>/dev/null || true
