#!/usr/bin/env bash
# Runs the relay in Docker on a VPS that already has Caddy serving other sites.
#
#   sudo ./deploy/docker-deploy.sh remote-mcp.example.com     # A record -> this VPS
#
# Unlike install.sh this leaves the existing Caddyfile alone: the site goes in
# /etc/caddy/remote-mcp.Caddyfile and is pulled in with one `import` line.
# Safe to re-run: secrets in /etc/remote-mcp/relay.env are kept, the image is
# rebuilt and the container replaced. Re-run after `git pull` to update.
set -euo pipefail

DOMAIN="${1:?usage: $0 <domain>}"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE=/etc/remote-mcp/relay.env
SITE_FILE=/etc/caddy/remote-mcp.Caddyfile
HOST_PORT="${HOST_PORT:-18080}"         # relay is published on 127.0.0.1 only
MACHINES="${MACHINES:-laptop labpc}"   # one agent token per machine name
NAME=remote-mcp

[[ $EUID -eq 0 ]] || { echo "run as root (sudo $0)"; exit 1; }
command -v docker >/dev/null || { echo "docker is not installed"; exit 1; }
command -v caddy >/dev/null || { echo "caddy is not installed"; exit 1; }

say() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
rand() { openssl rand -hex "${1:-24}"; }

# ---------- secrets ----------
install -d -m 700 /etc/remote-mcp
if [[ ! -f "$ENV_FILE" ]]; then
  say "Generating secrets"
  TOKENS=""
  for m in $MACHINES; do TOKENS+="${TOKENS:+,}$m=$(rand 24)"; done
  cat >"$ENV_FILE" <<EOF
PUBLIC_URL=https://$DOMAIN
# name=token pairs, one per machine that runs agent.exe
AGENT_TOKENS=$TOKENS
# password for the OAuth "Allow" page (claude.ai), the gallery and downloads
ADMIN_PASSWORD=$(rand 12)
# static bearer token for Claude Code / scripts (optional)
MCP_TOKEN=$(rand 24)
CAPTURE_TTL=72h
LABELED_TTL=720h
EOF
else
  sed -i -E "s#^PUBLIC_URL=.*#PUBLIC_URL=https://$DOMAIN#" "$ENV_FILE"
fi
chmod 600 "$ENV_FILE"

# ---------- image + container ----------
say "Building image"
docker build -t "$NAME" "$REPO_DIR"

say "Starting container"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --restart unless-stopped \
  --env-file "$ENV_FILE" \
  -p "127.0.0.1:$HOST_PORT:8080" \
  -v "$NAME-data:/data" \
  "$NAME" >/dev/null
docker image prune -f >/dev/null

# ---------- Caddy ----------
say "Configuring Caddy for $DOMAIN"
cat >"$SITE_FILE" <<EOF
# remote-mcp — written by deploy/docker-deploy.sh
$DOMAIN {
	reverse_proxy 127.0.0.1:$HOST_PORT {
		flush_interval -1
	}
}
EOF
if ! grep -qF "import $SITE_FILE" /etc/caddy/Caddyfile; then
  cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.before-remote-mcp"
  printf '\nimport %s\n' "$SITE_FILE" >>/etc/caddy/Caddyfile
fi
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null
systemctl reload caddy

# ---------- check ----------
say "Waiting for https://$DOMAIN"
for _ in $(seq 1 30); do
  if curl -fsS "https://$DOMAIN/healthz" >/dev/null 2>&1; then OK=1; break; fi
  sleep 2
done
[[ "${OK:-0}" == 1 ]] && echo "https://$DOMAIN is up" \
  || echo "WARNING: https://$DOMAIN/healthz not reachable yet; check DNS and: journalctl -u caddy -n 50; docker logs $NAME"

# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a
cat <<EOF

=====================================================================
 remote-mcp relay is running in Docker (container "$NAME").

 MCP endpoint (claude.ai -> Settings -> Connectors -> Add custom connector):
     https://$DOMAIN/mcp
 Admin password (Allow page, gallery, downloads):
     $ADMIN_PASSWORD
 Agent downloads (any user name + admin password):
     https://$DOMAIN/download/
 Claude Code:
     claude mcp add --transport http remote https://$DOMAIN/mcp \\
       --header "Authorization: Bearer $MCP_TOKEN"

 Secrets: $ENV_FILE   Logs: docker logs -f $NAME
=====================================================================
EOF
