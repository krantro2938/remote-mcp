#!/usr/bin/env bash
# Installs or updates the remote-mcp relay on a Debian/Ubuntu VPS.
#
#   git clone https://github.com/krantro2938/remote-mcp && cd remote-mcp
#   sudo ./deploy/install.sh                      # uses <ip>.sslip.io as the domain
#   sudo ./deploy/install.sh relay.example.com    # or your own domain (A record -> this VPS)
#
# Safe to re-run: secrets in /etc/remote-mcp/relay.env are kept, the binaries
# are rebuilt and the service is restarted. Re-run after `git pull` to update.
set -euo pipefail

GO_VERSION="1.24.7"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE=/etc/remote-mcp/relay.env
DATA_DIR=/var/lib/remote-mcp
MACHINES="${MACHINES:-laptop labpc}"   # one agent token per machine name

[[ $EUID -eq 0 ]] || { echo "run as root (sudo $0)"; exit 1; }
command -v apt-get >/dev/null || { echo "this script supports Debian/Ubuntu only"; exit 1; }

say() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
rand() { openssl rand -hex "${1:-24}"; }

say "Installing packages"
apt-get update -qq
apt-get install -y -qq curl ca-certificates openssl git debian-keyring debian-archive-keyring apt-transport-https gnupg >/dev/null

# ---------- domain ----------
DOMAIN="${1:-}"
if [[ -z "$DOMAIN" && -f "$ENV_FILE" ]]; then
  DOMAIN="$(grep -E '^PUBLIC_URL=' "$ENV_FILE" | sed -E 's#^PUBLIC_URL=https?://##')"
fi
if [[ -z "$DOMAIN" ]]; then
  IP="$(curl -4 -fsS https://api.ipify.org || curl -4 -fsS https://ifconfig.me)"
  DOMAIN="${IP//./-}.sslip.io"
fi
say "Domain: $DOMAIN"

# ---------- Go ----------
if ! command -v go >/dev/null || [[ "$(go env GOVERSION 2>/dev/null)" < "go1.24" ]]; then
  say "Installing Go $GO_VERSION"
  ARCH="$(dpkg --print-architecture)"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
fi

# ---------- build ----------
say "Building relay and agent.exe"
cd "$REPO_DIR"
export GOTOOLCHAIN=local CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o /usr/local/bin/remote-mcp-relay ./cmd/relay
mkdir -p dist
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/agent.exe ./cmd/agent
GOOS=windows GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/agent-arm64.exe ./cmd/agent

# ---------- user, dirs, secrets ----------
id remote-mcp >/dev/null 2>&1 || useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin remote-mcp
install -d -o remote-mcp -g remote-mcp -m 750 "$DATA_DIR" "$DATA_DIR/downloads"
install -d -m 750 /etc/remote-mcp

NEW_SECRETS=0
if [[ ! -f "$ENV_FILE" ]]; then
  say "Generating secrets"
  NEW_SECRETS=1
  TOKENS=""
  for m in $MACHINES; do TOKENS+="${TOKENS:+,}$m=$(rand 24)"; done
  cat >"$ENV_FILE" <<EOF
PUBLIC_URL=https://$DOMAIN
LISTEN_ADDR=127.0.0.1:8080
DATA_DIR=$DATA_DIR
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
chown root:remote-mcp "$ENV_FILE"
chmod 640 "$ENV_FILE"
# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a

# ---------- downloads (agent.exe + one agent.json per machine) ----------
cp dist/agent.exe dist/agent-arm64.exe "$DATA_DIR/downloads/"
IFS=',' read -ra PAIRS <<<"$AGENT_TOKENS"
for pair in "${PAIRS[@]}"; do
  name="${pair%%=*}"; tok="${pair#*=}"
  printf '{\n  "server": "https://%s",\n  "token": "%s",\n  "name": "%s"\n}\n' "$DOMAIN" "$tok" "$name" >"$DATA_DIR/downloads/agent-$name.json"
done
chown -R remote-mcp:remote-mcp "$DATA_DIR"
chmod 640 "$DATA_DIR"/downloads/*.json

# ---------- systemd ----------
say "Installing service"
install -m 644 "$REPO_DIR/deploy/remote-mcp.service" /etc/systemd/system/remote-mcp.service
systemctl daemon-reload
systemctl enable remote-mcp >/dev/null 2>&1
systemctl restart remote-mcp

# ---------- Caddy (HTTPS) ----------
if ! command -v caddy >/dev/null; then
  say "Installing Caddy"
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' >/etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq && apt-get install -y -qq caddy >/dev/null
fi
if [[ -f /etc/caddy/Caddyfile ]] && ! grep -q "remote-mcp" /etc/caddy/Caddyfile; then
  cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%s)"
fi
sed "s#{\$DOMAIN}#$DOMAIN#" "$REPO_DIR/deploy/Caddyfile" >/etc/caddy/Caddyfile
systemctl enable caddy >/dev/null 2>&1
systemctl reload caddy 2>/dev/null || systemctl restart caddy

if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  ufw allow 80/tcp >/dev/null; ufw allow 443/tcp >/dev/null
fi

# ---------- check ----------
say "Waiting for HTTPS certificate"
for i in $(seq 1 30); do
  if curl -fsS "https://$DOMAIN/healthz" >/dev/null 2>&1; then OK=1; break; fi
  sleep 2
done
[[ "${OK:-0}" == 1 ]] && echo "https://$DOMAIN is up" || echo "WARNING: https://$DOMAIN/healthz not reachable yet; check: journalctl -u caddy -n 50"

cat <<EOF

=====================================================================
 remote-mcp relay is running.

 MCP endpoint (claude.ai -> Settings -> Connectors -> Add custom connector):
     https://$DOMAIN/mcp
   leave OAuth client ID/secret empty; on the "Allow remote control?"
   page enter the admin password.

 Admin password (Allow page, gallery, downloads):
     $ADMIN_PASSWORD

 Agent downloads (any user name + admin password):
     https://$DOMAIN/download/
   -> agent.exe and agent-<machine>.json; save the json next to agent.exe
      as agent.json, then double-click agent.exe.

 Captures gallery:  https://$DOMAIN/gallery

 Claude Code:
     claude mcp add --transport http remote https://$DOMAIN/mcp \\
       --header "Authorization: Bearer $MCP_TOKEN"

 Secrets live in $ENV_FILE. Logs: journalctl -u remote-mcp -f
=====================================================================
EOF
