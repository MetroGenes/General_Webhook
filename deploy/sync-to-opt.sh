#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "error: run as root" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

captain_user="${CAPTAIN_USER:-captain}"
captain_group="${CAPTAIN_GROUP:-captain}"
webhook_dst="${WEBHOOK_DST:-/opt/general-webhook}"
webhook_data_dir="${WEBHOOK_DATA_DIR:-/data/webhook}"
webhook_uid="${WEBHOOK_UID:-10001}"
webhook_gid="${WEBHOOK_GID:-10001}"
webhook_internal_network="${WEBHOOK_INTERNAL_NETWORK:-webhook-internal}"
webhook_egress_network="${WEBHOOK_EGRESS_NETWORK:-webhook-egress}"
webhook_image="${WEBHOOK_IMAGE:-general-webhook:release-$(date -u +%Y%m%d%H%M%S)}"

dotenv_value() {
  local file="$1"
  local wanted="$2"
  local line
  local value=""

  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" == "$wanted="* ]]; then
      value="${line#*=}"
    fi
  done < "$file"
  printf '%s' "$value"
}

require_env_match() {
  local file="$1"
  local key="$2"
  local expected="$3"
  local actual
  actual="$(dotenv_value "$file" "$key")"
  if [[ "$actual" != "$expected" ]]; then
    echo "error: $key in $file does not match this run; pass the same value explicitly" >&2
    exit 1
  fi
}

validate_image_ref() {
  local image="$1"
  local tail="${image##*/}"
  if [[ ! "$image" =~ ^[A-Za-z0-9][A-Za-z0-9._:/-]*$ || "$image" == *@* || "$tail" != *:* ||
        "$tail" == *:latest || "$tail" == *:replace-with-version ]]; then
    echo "error: WEBHOOK_IMAGE must use a unique, non-latest version tag" >&2
    exit 1
  fi
}

validate_image_ref "$webhook_image"

command -v rsync >/dev/null || { echo "error: rsync not found" >&2; exit 1; }
command -v docker >/dev/null || { echo "error: docker not found" >&2; exit 1; }
command -v runuser >/dev/null || { echo "error: runuser not found" >&2; exit 1; }
command -v realpath >/dev/null || { echo "error: realpath not found" >&2; exit 1; }
id "$captain_user" >/dev/null || { echo "error: user not found: $captain_user" >&2; exit 1; }
getent group "$captain_group" >/dev/null || { echo "error: group not found: $captain_group" >&2; exit 1; }

if [[ "$webhook_dst" != /* || "$webhook_data_dir" != /* ]]; then
  echo "error: WEBHOOK_DST and WEBHOOK_DATA_DIR must be absolute paths" >&2
  exit 1
fi
repo_root="$(realpath -m -- "$repo_root")"
webhook_dst="$(realpath -m -- "$webhook_dst")"
webhook_data_dir="$(realpath -m -- "$webhook_data_dir")"
if [[ "$webhook_dst" == "/" || "$webhook_data_dir" == "/" ]]; then
  echo "error: deployment and data paths must not be /" >&2
  exit 1
fi
if [[ "$webhook_data_dir/" == "$webhook_dst/"* || "$webhook_dst/" == "$webhook_data_dir/"* ]]; then
  echo "error: deployment and data paths must not overlap" >&2
  exit 1
fi
if [[ "$webhook_dst/" == "$repo_root/"* || "$repo_root/" == "$webhook_dst/"* ||
      "$webhook_data_dir/" == "$repo_root/"* || "$repo_root/" == "$webhook_data_dir/"* ]]; then
  echo "error: deployment and data paths must not overlap the source repository" >&2
  exit 1
fi
if [[ ! "$webhook_uid" =~ ^[0-9]+$ || "$webhook_uid" == "0" ||
      ! "$webhook_gid" =~ ^[0-9]+$ || "$webhook_gid" == "0" ]]; then
  echo "error: WEBHOOK_UID and WEBHOOK_GID must be non-zero integers" >&2
  exit 1
fi
if [[ ! "$webhook_internal_network" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]]; then
  echo "error: invalid WEBHOOK_INTERNAL_NETWORK" >&2
  exit 1
fi

install -d -o "$captain_user" -g "$captain_group" -m 0750 "$webhook_dst"
if [[ -L "$webhook_dst/.env" ]]; then
  echo "error: refusing symlink: $webhook_dst/.env" >&2
  exit 1
fi
if [[ -f "$webhook_dst/.env" ]]; then
  require_env_match "$webhook_dst/.env" WEBHOOK_DATA_DIR "$webhook_data_dir"
  require_env_match "$webhook_dst/.env" WEBHOOK_UID "$webhook_uid"
  require_env_match "$webhook_dst/.env" WEBHOOK_GID "$webhook_gid"
  require_env_match "$webhook_dst/.env" WEBHOOK_INTERNAL_NETWORK "$webhook_internal_network"
  validate_image_ref "$(dotenv_value "$webhook_dst/.env" WEBHOOK_IMAGE)"
fi

runuser -u "$captain_user" -- docker info >/dev/null || {
  echo "error: user cannot access the Docker daemon: $captain_user" >&2
  exit 1
}

if docker network inspect "$webhook_internal_network" >/dev/null 2>&1; then
  if [[ "$(docker network inspect --format '{{.Internal}}' "$webhook_internal_network")" != "true" ]]; then
    echo "error: existing Docker network is not internal: $webhook_internal_network" >&2
    exit 1
  fi
else
  docker network create --driver bridge --internal "$webhook_internal_network" >/dev/null
fi

install -d -o "$webhook_uid" -g "$webhook_gid" -m 0700 "$webhook_data_dir"

rsync -a --delete \
  --exclude='/.git/' \
  --exclude='/.agents/' \
  --exclude='/.codex/' \
  --exclude='/.claude/' \
  --exclude='/.env' \
  --exclude='/data/' \
  --exclude='/server' \
  --exclude='/webhook' \
  "$repo_root/" "$webhook_dst/"

if [[ ! -f "$webhook_dst/.env" ]]; then
  umask 0077
  {
    echo "WEBHOOK_DATA_DIR=$webhook_data_dir"
    echo "WEBHOOK_UID=$webhook_uid"
    echo "WEBHOOK_GID=$webhook_gid"
    echo "TZ=Asia/Shanghai"
    echo "WEBHOOK_CPU_LIMIT=0.50"
    echo "WEBHOOK_MEMORY_LIMIT=256m"
    echo "WEBHOOK_MEMORY_SWAP_LIMIT=256m"
    echo "WEBHOOK_MEMORY_RESERVATION=64m"
    echo "WEBHOOK_PIDS_LIMIT=64"
    echo "WEBHOOK_GOMEMLIMIT=192MiB"
    echo "WEBHOOK_GOGC=50"
    echo "WEBHOOK_IMAGE=$webhook_image"
    echo "WEBHOOK_INTERNAL_NETWORK=$webhook_internal_network"
    echo "WEBHOOK_EGRESS_NETWORK=$webhook_egress_network"
    echo "GITHUB_SECRET="
    echo "FEISHU_BOT="
    echo "CUSTOM_TOKEN="
    echo "BT_TOKEN="
    echo "TG_BOT_TOKEN="
    echo "TG_CHAT_ID="
    echo "DINGTALK_TOKEN="
    echo "DINGTALK_WEBHOOK_TOKEN="
  } > "$webhook_dst/.env"
fi

chown -R "$captain_user:$captain_group" "$webhook_dst"
chown -R "$webhook_uid:$webhook_gid" "$webhook_data_dir"
chmod -R u=rwX,g=rX,o= "$webhook_dst"
chmod 0600 "$webhook_dst/.env"
chmod 0700 "$webhook_data_dir"

echo "synced:"
echo "  webhook:    $webhook_dst"
echo "  data:       $webhook_data_dir"
echo "  ingress:    $webhook_internal_network (internal)"

echo
echo "next:"
echo "  1. edit $webhook_dst/.env: verify the versioned image tag, deployment paths/limits, and only the sources you keep"
echo "  2. runuser -u $captain_user -- docker compose --project-directory $webhook_dst --env-file $webhook_dst/.env -f $webhook_dst/docker-compose.yaml config -q"
echo "  3. runuser -u $captain_user -- docker compose --project-directory $webhook_dst --env-file $webhook_dst/.env -f $webhook_dst/docker-compose.yaml build --pull"
echo "  4. runuser -u $captain_user -- docker compose --project-directory $webhook_dst --env-file $webhook_dst/.env -f $webhook_dst/docker-compose.yaml up -d --no-build --remove-orphans --wait --wait-timeout 60"
