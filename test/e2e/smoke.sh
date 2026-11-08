#!/usr/bin/env bash
# Smoke test: load the plugin in a real Gotify, send `ping`, expect `pong`,
# check the panel auth and survive a restart.
#
# Usage: test/e2e/smoke.sh <gotify-version> <path/to/plugin.so> [platform]
#   platform defaults to linux/amd64; use linux/arm64 with the arm64 .so and
#   the gotify/server-arm64 image on Apple Silicon to avoid emulation.
set -euo pipefail

GOTIFY_VERSION="${1:?gotify version, e.g. 3.1.1}"
PLUGIN_SO="$(cd "$(dirname "${2:?path to .so}")" && pwd)/$(basename "$2")"
PLATFORM="${3:-linux/amd64}"

HERE="$(cd "$(dirname "$0")" && pwd)"
PORT="${GOTIFY_PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
ADMIN_PASS="smoke-admin"
PANEL_PASS="smoke-panel-password"

# The arm64 image lives under another repository name.
IMAGE="gotify/server:${GOTIFY_VERSION}"
if [ "$PLATFORM" = "linux/arm64" ]; then
  IMAGE="gotify/server-arm64:${GOTIFY_VERSION}"
fi

export GOTIFY_IMAGE="$IMAGE" GOTIFY_PLATFORM="$PLATFORM" GOTIFY_PORT="$PORT"
export GOTIFY_ADMIN_PASS="$ADMIN_PASS" PLUGIN_SO
ARCH_TAG="${PLATFORM##*/}"
export GOTIFY_CONTAINER="gotify-commander-smoke-${GOTIFY_VERSION//./-}-${ARCH_TAG}"
# One compose project per run, so two versions can be tested side by side.
export COMPOSE_PROJECT_NAME="gotify-commander-smoke-${GOTIFY_VERSION//./-}-${ARCH_TAG}"

compose() { docker compose -f "$HERE/docker-compose.yml" "$@"; }
admin() { curl -fsS -u "admin:${ADMIN_PASS}" "$@"; }
fail() { echo "FAIL: $*" >&2; compose logs --tail=40 gotify >&2 || true; compose down -v >/dev/null 2>&1 || true; exit 1; }
step() { echo "== $*"; }

wait_health() {
  local tries=0
  until curl -fsS "${BASE}/health" >/dev/null 2>&1; do
    tries=$((tries + 1))
    [ "$tries" -lt 60 ] || fail "Gotify did not answer /health in 60 s"
    sleep 1
  done
}

trap 'compose down -v >/dev/null 2>&1 || true' EXIT

step "Gotify ${GOTIFY_VERSION} (${PLATFORM}) with $(basename "$PLUGIN_SO")"
compose down -v >/dev/null 2>&1 || true
compose up -d >/dev/null
wait_health
echo "health: $(curl -fsS "${BASE}/health")"

step "plugin listed"
PLUGINS="$(admin "${BASE}/plugin")"
PLUGIN_ID="$(echo "$PLUGINS" | jq -r '.[] | select(.modulePath=="github.com/drolosoft/gotify-commander") | .id')"
PLUGIN_TOKEN="$(echo "$PLUGINS" | jq -r '.[] | select(.modulePath=="github.com/drolosoft/gotify-commander") | .token')"
# The plugin list carries no version; the plugin prints its own in the display.
PLUGIN_NAME="$(echo "$PLUGINS" | jq -r '.[] | select(.modulePath=="github.com/drolosoft/gotify-commander") | .name')"
[ -n "$PLUGIN_ID" ] && [ "$PLUGIN_ID" != "null" ] || fail "plugin not loaded: $PLUGINS"
echo "plugin id=${PLUGIN_ID} name=${PLUGIN_NAME}"

step "client token and command app"
CLIENT_TOKEN="$(admin -H 'Content-Type: application/json' -d '{"name":"smoke-phone"}' "${BASE}/client" | jq -r .token)"
APP_JSON="$(admin -H 'Content-Type: application/json' -d '{"name":"commander"}' "${BASE}/application")"
APP_ID="$(echo "$APP_JSON" | jq -r .id)"
APP_TOKEN="$(echo "$APP_JSON" | jq -r .token)"
echo "app id=${APP_ID}"

step "configure and enable"
CONFIG="$(cat <<YAML
gotify:
  server_url: "http://localhost:80"
  client_token: "${CLIENT_TOKEN}"
  command_app_id: ${APP_ID}
web_password: "${PANEL_PASS}"
defaults:
  timeout: 30s
  log_lines: 30
categories:
  - label: "Info"
    type: direct
    commands: [help, ping]
YAML
)"
admin -X POST -H 'Content-Type: application/x-yaml' --data-binary "$CONFIG" "${BASE}/plugin/${PLUGIN_ID}/config" >/dev/null || fail "config rejected"
admin -X POST "${BASE}/plugin/${PLUGIN_ID}/enable" >/dev/null || fail "enable failed"
sleep 2
ENABLED="$(admin "${BASE}/plugin" | jq -r ".[] | select(.id==${PLUGIN_ID}) | .enabled")"
[ "$ENABLED" = "true" ] || fail "plugin not enabled after enable"
# /display answers a JSON string on some versions and an object on others.
DISPLAY="$(admin "${BASE}/plugin/${PLUGIN_ID}/display" | jq -r 'if type=="object" then .message else . end')"
# The version is stamped into the .so with -ldflags; read it from the file.
PLUGIN_VERSION="$(go version -m "$PLUGIN_SO" 2>/dev/null | sed -n 's/.*-X main.version=\([^ "]*\).*/\1/p' | head -1)"
echo "$DISPLAY" | grep -q 'web_password' || echo "note: display did not mention web_password: ${DISPLAY:0:160}"

step "send ping"
# On Gotify 3 a client token may post with an appid; on 2.x only the app token can.
MAJOR="${GOTIFY_VERSION%%.*}"
if [ "$MAJOR" -ge 3 ]; then
  curl -fsS -H "X-Gotify-Key: ${CLIENT_TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"message\":\"ping\",\"appid\":${APP_ID}}" "${BASE}/message" >/dev/null || fail "client token could not post with appid"
else
  curl -fsS -H "X-Gotify-Key: ${APP_TOKEN}" -H 'Content-Type: application/json' \
    -d '{"message":"ping"}' "${BASE}/message" >/dev/null || fail "app token could not post"
fi

step "wait for pong"
tries=0
until admin "${BASE}/message?limit=20" | jq -e '.messages[] | select(.extras["commander::response"]==true) | select(.message|test("pong";"i"))' >/dev/null 2>&1; do
  tries=$((tries + 1))
  [ "$tries" -lt 20 ] || fail "no pong within 20 s: $(admin "${BASE}/message?limit=20" | jq -c '.messages[] | {title, message}')"
  sleep 1
done
admin "${BASE}/message?limit=20" | jq -c '.messages[] | select(.extras["commander::response"]==true) | {title, message}' | head -3

step "panel auth"
PANEL="${BASE}/plugin/${PLUGIN_ID}/custom/${PLUGIN_TOKEN}"
NOAUTH="$(curl -s -o /dev/null -w '%{http_code}' "${PANEL}/health")"
WITHAUTH="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer ${PANEL_PASS}" "${PANEL}/health")"
QUERY="$(curl -s -o /dev/null -w '%{http_code}' "${PANEL}/health?token=${PANEL_PASS}")"
PAGE="$(curl -s -o /dev/null -w '%{http_code}' "${PANEL}/")"
CSP="$(curl -s -D - -o /dev/null "${PANEL}/" | grep -ci 'content-security-policy' || true)"
echo "no bearer=${NOAUTH} bearer=${WITHAUTH} query=${QUERY} page=${PAGE} csp-header=${CSP}"
[ "$NOAUTH" = "401" ] || fail "panel without Bearer gave ${NOAUTH}"
[ "$WITHAUTH" = "200" ] || fail "panel with Bearer gave ${WITHAUTH}"
[ "$QUERY" = "401" ] || fail "?token= still accepted (${QUERY})"
[ "$PAGE" = "200" ] && [ "$CSP" -ge 1 ] || fail "panel page ${PAGE}, CSP headers ${CSP}"

step "restart Gotify"
compose restart gotify >/dev/null
wait_health
# Post the ping at once, while the plugin is still dialling the stream with
# its backoff. Gotify does not push stored messages to a client that connects
# later; the plugin reads them on connect and must answer this one ping
# without it being sent again.
curl -fsS -H "X-Gotify-Key: ${APP_TOKEN}" -H 'Content-Type: application/json' -d '{"message":"ping"}' "${BASE}/message" >/dev/null || fail "could not post ping after restart"
ENABLED="$(admin "${BASE}/plugin" | jq -r ".[] | select(.id==${PLUGIN_ID}) | .enabled")"
[ "$ENABLED" = "true" ] || fail "plugin not enabled after restart"
tries=0
until [ "$(admin "${BASE}/message?limit=30" | jq '[.messages[] | select(.extras["commander::response"]==true) | select(.message|test("pong";"i"))] | length')" -ge 2 ]; do
  tries=$((tries + 1))
  [ "$tries" -le 30 ] || fail "no second pong within 30 s of the restart: $(compose logs --since 2m gotify 2>&1 | grep '\[stream\]' | tail -5)"
  sleep 1
done
echo "plugin answered the ping posted during the restart after ${tries} s"
compose logs gotify 2>&1 | grep -E '\[stream\] (connected|replayed|first start|connection error)' | tail -4 || true

echo "OK: gotify ${GOTIFY_VERSION} ${PLATFORM} plugin ${PLUGIN_VERSION}"
