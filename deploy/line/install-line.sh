#!/bin/bash
# Install or update one phone line on a server, from your workstation (needs Go 1.21+, ssh, scp, curl). One command, idempotent.
#
#   install-line.sh <line>             build, ship, start, point the number, live check
#   install-line.sh <line> --secrets   also rewrite the line's secrets file (needs lines/<line>/secrets.sh)
#   install-line.sh <line> --no-check  skip the live check (it spends two short legs of call minutes)
#
# A line is a folder in deploy/lines/<line>/:
#   line.env       where it runs: HOST, LINE_DIR, INGRESS_DIR, LINE_PROJECT, LINE_CONTAINER,
#                  LINE_NET, LINE_PORT, LINE_HOSTNAME, LINE_SECRETS, optional AGENT_CONTAINER, LINE_TZ
#   dialout.yaml   the line's config (owner, number, allowlist, limits, profiles)
#   prompt.tmpl    optional prompt file, installed next to the config
#   secrets.sh     optional: prints the secrets file on stdout (e.g. keys read from a keychain).
#                  Without it the secrets file must already be on the box (LINE_SECRETS, root 600).
#
# The box needs Docker, sudo without a password for the ssh user, a public address, and ports
# 80 and 443 free. No tunnel and no tunnel account: the phone ingress (deploy/ingress, Caddy)
# gets its own certificate for LINE_HOSTNAME and forwards only Twilio's paths to the line.
# Never prints a secret: values go from secrets.sh straight into ssh's stdin.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; REPO="$(cd "$HERE/../.." && pwd)"
LINE="${1:?usage: install-line.sh <line> [--secrets] [--no-check]}"; shift || true
WRITE_SECRETS=0; CHECK=1
for a in "$@"; do case "$a" in --secrets) WRITE_SECRETS=1 ;; --no-check) CHECK=0 ;; *) echo "unknown flag $a" >&2; exit 2 ;; esac; done
L="$REPO/deploy/lines/$LINE"
[ -f "$L/line.env" ] && [ -f "$L/dialout.yaml" ] || { echo "no line $LINE: need $L/line.env and dialout.yaml" >&2; exit 2; }
AGENT_CONTAINER=""; LINE_TZ=""
# shellcheck disable=SC1091
. "$L/line.env"
for v in HOST LINE_DIR INGRESS_DIR LINE_PROJECT LINE_CONTAINER LINE_NET LINE_PORT LINE_HOSTNAME LINE_SECRETS; do
  [ -n "${!v:-}" ] || { echo "line.env: $v is empty" >&2; exit 2; }
done
grep -q "hostname: *$LINE_HOSTNAME" "$L/dialout.yaml" || { echo "dialout.yaml: tunnel.hostname must be $LINE_HOSTNAME (line.env)" >&2; exit 2; }
GO_DIR="$REPO"; [ -f "$REPO/go.mod" ] || GO_DIR="$REPO/dialout"   # the Go module: the repo root, or dialout/
TAG="$(git -C "$REPO" rev-parse --short HEAD)$(git -C "$REPO" diff --quiet HEAD -- "$GO_DIR" || echo -dirty)"
T=/tmp/agent-phone-build-$LINE

echo "[$LINE] build $TAG (linux/amd64)"
mkdir -p "$HERE/build"
(cd "$GO_DIR" && GOTOOLCHAIN=go1.27.0 CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=agent-phone-$TAG" -o "$HERE/build/dialout" ./cmd/dialout)

echo "[$LINE] ship to $HOST"
ssh "$HOST" "rm -rf $T && mkdir -p $T/ingress"
scp -q "$HERE/build/dialout" "$HERE/Dockerfile" "$HERE/compose.yaml" "$L/dialout.yaml" "$HOST:$T/"
[ ! -f "$L/prompt.tmpl" ] || scp -q "$L/prompt.tmpl" "$HOST:$T/"
scp -q "$REPO/deploy/ingress/compose.yaml" "$REPO/deploy/ingress/Caddyfile" "$HOST:$T/ingress/"
ssh "$HOST" "set -e
  sudo -n mkdir -p $LINE_DIR/config $LINE_DIR/data $INGRESS_DIR/sites $INGRESS_DIR/data
  sudo -n chown 65532:65532 $LINE_DIR/data && sudo -n chmod 700 $LINE_DIR/data $INGRESS_DIR/data
  sudo -n install -o root -g root -m 644 $T/dialout.yaml $LINE_DIR/config/dialout.yaml
  [ ! -f $T/prompt.tmpl ] || sudo -n install -o root -g root -m 644 $T/prompt.tmpl $LINE_DIR/config/prompt.tmpl
  sudo -n install -o root -g root -m 644 $T/compose.yaml $LINE_DIR/compose.yaml
  sudo -n install -o root -g root -m 644 $T/ingress/compose.yaml $INGRESS_DIR/compose.yaml
  sudo -n install -o root -g root -m 644 $T/ingress/Caddyfile $INGRESS_DIR/Caddyfile
  printf '%s\n' '# line $LINE: Twilio webhook and media stream only (written by install-line.sh)' \
    '$LINE_HOSTNAME {' '	@twilio path /twilio/* /healthz' '	handle @twilio {' '		reverse_proxy 127.0.0.1:$LINE_PORT' '	}' \
    '	handle {' '		respond 404' '	}' '}' | sudo -n tee $INGRESS_DIR/sites/$LINE.caddy >/dev/null"

if [ "$WRITE_SECRETS" = 1 ] || ! ssh "$HOST" "sudo -n test -s $LINE_SECRETS"; then
  [ -x "$L/secrets.sh" ] || { echo "[$LINE] no secrets on the box at $LINE_SECRETS and no lines/$LINE/secrets.sh to write them" >&2; exit 1; }
  echo "[$LINE] secrets (not printed)"
  "$L/secrets.sh" | ssh "$HOST" "sudo -n install -o root -g root -m 600 /dev/stdin $LINE_SECRETS"
fi

echo "[$LINE] ingress: ports 80 and 443, certificate for $LINE_HOSTNAME"
ssh "$HOST" "set -e
  if sudo -n ufw status 2>/dev/null | grep -q '^Status: active'; then
    sudo -n ufw allow 80/tcp comment 'phone-ingress acme' >/dev/null; sudo -n ufw allow 443/tcp comment 'phone-ingress twilio' >/dev/null
  fi
  cd $INGRESS_DIR && sudo -n env INGRESS_DIR=$INGRESS_DIR docker compose -f compose.yaml up -d 2>&1 | grep -v -E 'Running|up-to-date' || true
  sudo -n docker exec phone-ingress caddy reload --config /etc/caddy/Caddyfile --address unix//run/admin.sock 2>&1 | grep -i -E 'error' || true"

echo "[$LINE] image + start"
ssh "$HOST" "set -e
  cd $T && sudo -n docker build -q -t agent-phone:$TAG . >/dev/null 2>&1
  sudo -n docker network inspect $LINE_NET >/dev/null 2>&1 || sudo -n docker network create $LINE_NET >/dev/null
  cd $LINE_DIR && sudo -n env AGENT_PHONE_TAG=$TAG LINE_PROJECT=$LINE_PROJECT LINE_CONTAINER=$LINE_CONTAINER LINE_NET=$LINE_NET \
    LINE_PORT=$LINE_PORT LINE_DIR=$LINE_DIR LINE_SECRETS=$LINE_SECRETS LINE_TZ=${LINE_TZ:-UTC} docker compose -f compose.yaml up -d --remove-orphans 2>&1 | tail -4
  rm -rf $T"
if [ -n "$AGENT_CONTAINER" ]; then
  ssh "$HOST" "sudo -n docker inspect -f '{{json .NetworkSettings.Networks}}' $AGENT_CONTAINER | grep -q '\"$LINE_NET\"' || sudo -n docker network connect $LINE_NET $AGENT_CONTAINER"
fi

echo "[$LINE] wait for the line and its certificate"
ok=0
for _ in $(seq 1 30); do
  if [ "$(curl -s -o /dev/null -m 5 -w '%{http_code}' "https://$LINE_HOSTNAME/healthz" || true)" = 204 ]; then ok=1; break; fi
  sleep 3
done
[ "$ok" = 1 ] || { echo "[$LINE] FAIL: https://$LINE_HOSTNAME/healthz did not answer 204 in 90 s. Look at: ssh $HOST sudo docker logs phone-ingress; sudo docker logs $LINE_CONTAINER" >&2; exit 1; }

echo "[$LINE] point the number at https://$LINE_HOSTNAME/twilio/voice"
ssh "$HOST" "sudo -n docker exec $LINE_CONTAINER dialout number -point"

if [ "$CHECK" = 1 ]; then
  echo "[$LINE] live check: the line calls its own number and answers itself"
  ssh "$HOST" "sudo -n docker exec $LINE_CONTAINER dialout check"
fi
ssh "$HOST" "sudo -n docker ps --filter name=$LINE_CONTAINER --filter name=phone-ingress --format '{{.Names}} {{.Status}} {{.Image}}'"
echo "[$LINE] done. MCP for the agent: http://$LINE_CONTAINER:8080/mcp on Docker network $LINE_NET, bearer DIALOUT_MCP_TOKEN from $LINE_SECRETS"
