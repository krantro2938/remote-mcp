#!/bin/sh
# Publishes agent.exe and one agent-<machine>.json per AGENT_TOKENS entry in
# $DATA_DIR/downloads (served at /download/), then starts the relay.
set -eu
dl="$DATA_DIR/downloads"
mkdir -p "$dl"
cp /opt/remote-mcp/agent.exe /opt/remote-mcp/agent-arm64.exe "$dl/"
IFS=','
for pair in ${AGENT_TOKENS:-}; do
  name="$(echo "${pair%%=*}" | tr -d ' ')"
  tok="$(echo "${pair#*=}" | tr -d ' ')"
  [ -n "$name" ] || continue
  printf '{\n  "server": "%s",\n  "token": "%s",\n  "name": "%s"\n}\n' "$PUBLIC_URL" "$tok" "$name" >"$dl/agent-$name.json"
  chmod 640 "$dl/agent-$name.json"
done
unset IFS
exec remote-mcp-relay "$@"
