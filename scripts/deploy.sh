#!/usr/bin/env bash
# deploy.sh — ship gochathub-server to production.
#
# Deploys HEAD (git archive), so committed-and-pushed code only. Working-tree
# changes are not deployed. Edge Caddyfile edits stay manual (see
# memory/past rollout: validate with caddy:2-alpine, keep .bak-*); any new
# non-/api route also needs a handle in the prod edge Caddyfile.
#
# Usage: scripts/deploy.sh
set -euo pipefail

HOST="${DEPLOY_HOST:-cloud.tafoyaventures.com}"
DIR="${DEPLOY_DIR:-/opt/stacks/gochathub}"
URL="${DEPLOY_URL:-https://chat.tafoyaventures.com}"

cd "$(git rev-parse --show-toplevel)"

# Archive is built from HEAD; refuse instead of silently deploying stale code.
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "error: uncommitted tracked changes. Commit or stash first (git archive deploys HEAD only)." >&2
  exit 1
fi
SHA="$(git rev-parse --short HEAD)"
echo "Deploying $SHA"

echo "1/4 build"
git archive HEAD | docker build -t gochathub-server:"$SHA" -q -

echo "2/4 push image"
docker save gochathub-server:"$SHA" | ssh "$HOST" docker load

echo "3/4 backup, swap, migrate, restart"
ssh "$HOST" bash -s "$SHA" "$DIR" <<'EOF'
set -euo pipefail
SHA="$1"; DIR="$2"
cd "$DIR"
trap 'echo "FAILED. Rollback: docker tag gochathub-server:prev gochathub-server:prod && docker compose up -d server (DB restore from backups/pre-deploy-*.dump)"' ERR

ts=$(date +%Y%m%d-%H%M%S)
mkdir -p backups
umask 077
docker compose exec -T postgres pg_dump -U gochathub -Fc gochathub > "backups/pre-deploy-$SHA-$ts.dump" </dev/null
echo "backup: backups/pre-deploy-$SHA-$ts.dump"

docker tag gochathub-server:prod gochathub-server:prev
docker tag gochathub-server:"$SHA" gochathub-server:prod

# ponytail: -T + </dev/null on every compose run/exec — a bare compose run
# forwards ssh stdin into the container and eats what's left of this script.
docker compose run --rm -T server migrate </dev/null
docker compose up -d server </dev/null

docker image inspect --format '{{.Id}}' gochathub-server:prod | grep -qF "$(docker image inspect --format '{{.Id}}' gochathub-server:"$SHA")" || {
  echo "::error::prod tag does not point at the new image; retag failed"
  exit 1
}
started=$(docker compose ps --format '{{.Image}} {{.Status}}' server)
case "$started" in *"gochathub-server:prod Up"*) ;; *) echo "::error::server not running new image: $started"; exit 1;; esac
echo "remote done: $started"
EOF

echo "4/4 verify"
curl -fsS "$URL/healthz" >/dev/null
curl -fsS "$URL/readyz" >/dev/null
echo "$SHA deployed, /healthz and /readyz OK via $URL"
echo "Next: spot-check $URL and server logs on $HOST."