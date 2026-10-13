#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  echo "Usage: full-backup.sh --config FILE|- [--check]"
  echo "Read deployment JSON supplied by the agent; --check performs read-only preflight."
}
die() {
  echo "ERROR: $*" >&2
  exit 1
}
CONFIG_FILE=
CHECK=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --config)
      [ "$#" -ge 2 ] || die "--config requires a file or -"
      CONFIG_FILE=$2
      shift 2
      ;;
    --check) CHECK=1; shift ;;
    --help) usage; exit 0 ;;
    *) die "Unknown argument: $1" ;;
  esac
done
[ -n "$CONFIG_FILE" ] || { usage >&2; die "--config is required"; }
[ "$EUID" -eq 0 ] || die "Run as root"
command -v jq >/dev/null || die "jq is required"
if [ "$CONFIG_FILE" = "-" ]; then
  CONFIG=$(cat)
else
  CONFIG=$(cat -- "$CONFIG_FILE")
fi
jq -e '
  type == "object" and
  ([
    .app_dir, .compose_file, .env_file, .proxy_config_file, .destination,
    .owner, .group, .postgres_container, .backend_container, .uploads_volume,
    .archive_image, .recipient_file, .gpg_home, .zstd_binary, .docker_binary,
    .health_url, .lock_file, .compose_project, .host, .storage_mount, .storage_source
  ] | all(.[]; type == "string" and length > 0 and (test("[\u0000-\u001f]") | not))) and
  (.services | type == "array" and length > 0 and
    all(.[]; type == "string" and test("^[A-Za-z0-9][A-Za-z0-9_.-]*$"))) and
  (.keep_days | type == "number" and . >= 1 and . <= 36500 and . == floor) and
  (.min_free_bytes | type == "number" and . >= 1 and . == floor) and
  ([.app_dir, .destination, .recipient_file, .gpg_home, .zstd_binary,
    .docker_binary, .lock_file, .storage_mount] |
    all(.[]; startswith("/") and (split("/") | all(.[]; . != ".." and . != ".")))) and
  ([.compose_file, .env_file, .proxy_config_file] |
    all(.[]; (startswith("/") | not) and
      (split("/") | all(.[]; . != ".." and . != "." and length > 0)))) and
  ([.postgres_container, .backend_container, .uploads_volume, .owner, .group,
    .compose_project] | all(.[]; test("^[A-Za-z0-9_][A-Za-z0-9_.-]*$"))) and
  (.archive_image | test("^[A-Za-z0-9][A-Za-z0-9_./:@-]*$")) and
  (.health_url | test("^https?://")) and
  (.destination != "/" and .destination != .app_dir)
' <<< "$CONFIG" >/dev/null || die "Invalid or incomplete deployment JSON"
value() { jq -r --arg key "$1" '.[$key]' <<< "$CONFIG"; }
[ "$(hostname -s)" = "$(value host)" ] || die "Deployment host does not match this machine"
APP_DIR=$(value app_dir)
COMPOSE_NAME=$(value compose_file)
ENV_NAME=$(value env_file)
PROXY_NAME=$(value proxy_config_file)
COMPOSE_FILE="$APP_DIR/$COMPOSE_NAME"
ENV_FILE="$APP_DIR/$ENV_NAME"
DEST=$(value destination)
KEEP_DAYS=$(value keep_days)
OWNER=$(value owner)
GROUP=$(value group)
POSTGRES_CONTAINER=$(value postgres_container)
BACKEND_CONTAINER=$(value backend_container)
UPLOADS_VOLUME=$(value uploads_volume)
ARCHIVE_IMAGE=$(value archive_image)
RECIPIENT_FILE=$(value recipient_file)
GPG_HOME=$(value gpg_home)
ZSTD=$(value zstd_binary)
DOCKER=$(value docker_binary)
HEALTH_URL=$(value health_url)
LOCK_FILE=$(value lock_file)
COMPOSE_PROJECT=$(value compose_project)
STORAGE_MOUNT=$(value storage_mount)
STORAGE_SOURCE=$(value storage_source)
MIN_FREE_BYTES=$(value min_free_bytes)
mapfile -t SERVICES < <(jq -r '.services[]' <<< "$CONFIG")

compose() {
  "$DOCKER" compose --project-name "$COMPOSE_PROJECT" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"
}
for tool in tar gzip gpg curl flock stat install mktemp sha256sum find findmnt df \
  hostname getent id grep dirname basename date tr cat rm mv ln chown seq sleep tail; do
  command -v "$tool" >/dev/null || die "Missing required tool: $tool"
done
id "$OWNER" >/dev/null || die "Backup owner does not exist"
getent group "$GROUP" >/dev/null || die "Backup group does not exist"
for file in "$ENV_FILE" "$COMPOSE_FILE" "$APP_DIR/$PROXY_NAME" "$RECIPIENT_FILE"; do
  [ -r "$file" ] || die "Required input is not readable: $file"
done
[ -x "$ZSTD" ] || die "zstd executable unavailable: $ZSTD"
[ -x "$DOCKER" ] || die "Docker executable unavailable: $DOCKER"
[ -d "$(dirname "$DEST")" ] || die "Destination parent must already exist"
STORAGE_PATH=$(dirname "$DEST")
if [ -d "$DEST" ]; then STORAGE_PATH=$DEST; fi
[ "$(findmnt -rn -T "$STORAGE_PATH" -o TARGET)" = "$STORAGE_MOUNT" ] ||
  die "Destination is not on the configured storage mount"
[ "$(findmnt -rn -T "$STORAGE_PATH" -o SOURCE)" = "$STORAGE_SOURCE" ] ||
  die "Storage source differs from deployment configuration"
AVAILABLE_BYTES=$(df -B1 --output=avail "$STORAGE_PATH" | tail -n 1 | tr -d ' ')
[[ "$AVAILABLE_BYTES" =~ ^[0-9]+$ ]] || die "Cannot determine available storage"
[ "$AVAILABLE_BYTES" -ge "$MIN_FREE_BYTES" ] || die "Insufficient backup storage"
if [ -e "$DEST" ] || [ -L "$DEST" ]; then
  [ -d "$DEST" ] && [ ! -L "$DEST" ] || die "Destination must be a real directory"
  [ "$(stat -c %U "$DEST")" = "$OWNER" ] || die "Destination has incorrect owner"
  [ -w "$DEST" ] || die "Destination is not writable"
fi
RECIPIENT=$(tr -d '[:space:]' < "$RECIPIENT_FILE")
[ -n "$RECIPIENT" ] || die "Recipient file is empty"
gpg --homedir "$GPG_HOME" --batch --list-keys "$RECIPIENT" >/dev/null ||
  die "Recipient public key unavailable"
"$DOCKER" info >/dev/null || die "Docker daemon unavailable"
compose config --quiet
AVAILABLE_SERVICES=$(compose config --services)
for service in "${SERVICES[@]}"; do
  grep -Fxq "$service" <<< "$AVAILABLE_SERVICES" || die "Compose service not found: $service"
done
[ "$("$DOCKER" inspect "$POSTGRES_CONTAINER" --format '{{.State.Running}}')" = true ] ||
  die "Postgres container must be running"
"$DOCKER" inspect "$BACKEND_CONTAINER" >/dev/null
"$DOCKER" volume inspect "$UPLOADS_VOLUME" >/dev/null
"$DOCKER" image inspect "$ARCHIVE_IMAGE" >/dev/null ||
  die "Archive image must already be available locally"
"$DOCKER" exec "$POSTGRES_CONTAINER" sh -lc \
  'test -n "$POSTGRES_USER" && test -n "$POSTGRES_DB" && command -v pg_dump && command -v pg_dumpall && command -v psql' >/dev/null
if [ "$CHECK" -eq 1 ]; then
  echo "Preflight passed; no backup created and no services stopped."
  exit 0
fi

exec 9>"$LOCK_FILE"
flock -n 9 || die "Another backup is running"

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
WORK=
PLAIN="$DEST/.multica-full-${STAMP}.tar.zst"
FINAL="$DEST/multica-full-${STAMP}.tar.zst.gpg"
LATEST_TEMP="$DEST/.latest.tar.zst.gpg.${STAMP}"
STOPPED=0
HEALTHY=0

restore_services() {
  compose up -d "${SERVICES[@]}" || return 1
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 3 "$HEALTH_URL" >/dev/null; then
      HEALTHY=1
      STOPPED=0
      return 0
    fi
    sleep 2
  done
  echo "ERROR: Services started but health check failed" >&2
  return 1
}

cleanup() {
  rc=$?
  trap - EXIT INT TERM
  set +e
  if [ "$STOPPED" -eq 1 ]; then
    if ! restore_services; then
      echo "ERROR: Business recovery failed; manual intervention required" >&2
      rc=1
    fi
  fi
  if [ "$HEALTHY" -eq 1 ]; then
    echo "Business health: OK"
  fi
  if [ -n "$WORK" ]; then
    rm -rf -- "$WORK"
  fi
  rm -f -- "$PLAIN" "$FINAL.part" "$LATEST_TEMP"
  exit "$rc"
}
trap 'rc=$?; echo "ERROR: backup failed at line $LINENO (exit $rc)" >&2' ERR
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ ! -e "$DEST" ] && [ ! -L "$DEST" ]; then
  install -d -m 0700 -o "$OWNER" -g "$GROUP" "$DEST"
fi
test -d "$DEST"
test ! -L "$DEST"
test "$(stat -c %U "$DEST")" = "$OWNER"
[ ! -e "$FINAL" ] || die "Backup file already exists for this timestamp"
WORK=$(mktemp -d "$DEST/.work-${STAMP}-XXXXXX")

STOPPED=1
compose stop "${SERVICES[@]}"

"$DOCKER" exec "$POSTGRES_CONTAINER" sh -lc \
  'exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc --no-owner --no-acl' \
  > "$WORK/database.dump"

"$DOCKER" exec "$POSTGRES_CONTAINER" sh -lc \
  'exec pg_dumpall -U "$POSTGRES_USER" --globals-only' \
  > "$WORK/globals.sql"

"$DOCKER" run --rm --network none \
  --mount "type=volume,source=$UPLOADS_VOLUME,target=/source,readonly" \
  "$ARCHIVE_IMAGE" \
  tar -C /source -czf - . > "$WORK/uploads.tar.gz"

tar -C "$APP_DIR" -czf "$WORK/config.tar.gz" \
  "$ENV_NAME" "$COMPOSE_NAME" "$PROXY_NAME"

{
  echo "created_at_utc=$STAMP"
  echo "compose_project=$COMPOSE_PROJECT"
  echo "postgres_image=$("$DOCKER" inspect "$POSTGRES_CONTAINER" --format '{{.Config.Image}}')"
  echo "backend_image=$("$DOCKER" inspect "$BACKEND_CONTAINER" --format '{{.Config.Image}}')"
  "$DOCKER" exec "$POSTGRES_CONTAINER" sh -lc \
    'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "select version()"'
} > "$WORK/manifest.txt"

(cd "$WORK" && sha256sum database.dump globals.sql uploads.tar.gz config.tar.gz) \
  > "$WORK/CONTENTS.sha256"

tar -C "$WORK" -cf - \
  database.dump globals.sql uploads.tar.gz config.tar.gz manifest.txt CONTENTS.sha256 \
  | "$ZSTD" -T0 -10 -o "$PLAIN"
"$ZSTD" -t "$PLAIN"

gpg --homedir "$GPG_HOME" --batch --yes --trust-model always \
  --recipient "$RECIPIENT" --encrypt --output "$FINAL.part" "$PLAIN"
mv "$FINAL.part" "$FINAL"
(cd "$DEST" && sha256sum "$(basename "$FINAL")") > "$FINAL.sha256"
(cd "$DEST" && sha256sum -c "$(basename "$FINAL").sha256")
restore_services

chown "$OWNER:$GROUP" "$FINAL" "$FINAL.sha256"
ln -s "$(basename "$FINAL")" "$LATEST_TEMP"
chown -h "$OWNER:$GROUP" "$LATEST_TEMP"
mv -Tf "$LATEST_TEMP" "$DEST/latest.tar.zst.gpg"

find "$DEST" -maxdepth 1 -type f \
  \( -name 'multica-full-*.tar.zst.gpg' -o -name 'multica-full-*.tar.zst.gpg.sha256' \) \
  -mtime "+$KEEP_DAYS" -delete

echo "Backup completed: $FINAL"
