#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT_DIR/scripts/full-backup.sh"
if [ "$EUID" -ne 0 ]; then
  echo "Run this isolated fixture test as root (no live Docker or GPG required)." >&2
  exit 1
fi
TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/app" "$TMP/keys"
export CALLS="$TMP/calls"
export FAIL_AT=

cat > "$TMP/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "$CALLS"
case "$1" in
  info|volume|image) exit 0 ;;
  inspect)
    if [[ "$*" == *State.Running* ]]; then echo true; fi
    if [[ "$*" == *Config.Image* ]]; then echo fixture-image; fi
    ;;
  compose)
    if [[ "$*" == *"config --services"* ]]; then printf 'frontend\nbackend\n'; fi
    if [[ "$*" == *"stop frontend backend"* && "$FAIL_AT" == stop ]]; then exit 17; fi
    ;;
  exec)
    if [[ "$*" == *"command -v"* ]]; then exit 0; fi
    if [[ "$*" == *pg_dump* && "$FAIL_AT" == dump ]]; then exit 18; fi
    echo fixture-database
    ;;
  run) echo fixture-uploads ;;
  *) exit 99 ;;
esac
MOCK
cat > "$TMP/bin/gpg" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *--list-keys* ]]; then exit 0; fi
if [ "$FAIL_AT" = encrypt ]; then exit 19; fi
OUTPUT=
while [ "$#" -gt 1 ]; do
  if [ "$1" = --output ]; then OUTPUT=$2; shift 2; else shift; fi
done
cp -- "$1" "$OUTPUT"
MOCK
cat > "$TMP/bin/zstd" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = -t ]; then test -s "$2"; exit; fi
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then cat > "$2"; exit; fi
  shift
done
exit 99
MOCK
cat > "$TMP/bin/curl" <<'MOCK'
#!/usr/bin/env bash
[ "$FAIL_AT" != health ]
MOCK
cat > "$TMP/bin/sleep" <<'MOCK'
#!/usr/bin/env bash
exit 0
MOCK
chmod +x "$TMP/bin/"*
export PATH="$TMP/bin:$PATH"
touch "$TMP/app/env" "$TMP/app/compose.yml" "$TMP/app/proxy.conf"
echo fixture-recipient > "$TMP/recipient"
jq -n --arg root "$TMP" --arg host "$(hostname -s)" \
  --arg owner "$(id -un)" --arg group "$(id -gn)" \
  --arg mount "$(findmnt -rn -T "$TMP" -o TARGET)" \
  --arg source "$(findmnt -rn -T "$TMP" -o SOURCE)" '{
    app_dir: ($root+"/app"), host: $host, compose_project: "fixture",
    compose_file: "compose.yml", env_file: "env", proxy_config_file: "proxy.conf",
    destination: ($root+"/backup"), owner: $owner, group: $group, keep_days: 14,
    postgres_container: "fixture-postgres", backend_container: "fixture-backend",
    services: ["frontend","backend"], uploads_volume: "fixture-uploads",
    archive_image: "fixture-image", recipient_file: ($root+"/recipient"),
    gpg_home: ($root+"/keys"), zstd_binary: ($root+"/bin/zstd"),
    docker_binary: ($root+"/bin/docker"), health_url: "http://localhost/health",
    lock_file: ($root+"/backup.lock"), storage_mount: $mount,
    storage_source: $source, min_free_bytes: 1
  }' > "$TMP/config.json"

reject() {
  if bash "$SCRIPT" "$@" > "$TMP/result" 2>&1; then
    echo "Unexpected success: $*" >&2
    exit 1
  fi
}
reject
reject --unknown
for field in host app_dir services storage_mount min_free_bytes; do
  jq --arg field "$field" 'del(.[$field])' "$TMP/config.json" > "$TMP/bad.json"
  reject --config "$TMP/bad.json" --check
done
for expression in '.host="different-host"' '.storage_source="different-disk"' \
  '.min_free_bytes=999999999999999' '.compose_file="../escape"' '.destination="/"'; do
  jq "$expression" "$TMP/config.json" > "$TMP/bad.json"
  reject --config "$TMP/bad.json" --check
done
bash "$SCRIPT" --config "$TMP/config.json" --check
cat "$TMP/config.json" | bash "$SCRIPT" --config - --check
test ! -e "$TMP/backup"
test ! -e "$TMP/backup.lock"
! grep -q 'stop ' "$CALLS"
ln -s "$TMP/missing" "$TMP/backup"
reject --config "$TMP/config.json" --check
rm "$TMP/backup"

for stage in stop dump encrypt health; do
  export FAIL_AT=$stage
  : > "$CALLS"
  reject --config "$TMP/config.json"
  grep -q 'up -d frontend backend' "$CALLS"
  test ! -e "$TMP/backup/latest.tar.zst.gpg"
  test -z "$(find "$TMP/backup" -maxdepth 1 -name '.work-*' -o -name '*.part')"
  if [ "$stage" = health ]; then
    grep -q 'Business recovery failed' "$TMP/result"
  else
    grep -q 'Business health: OK' "$TMP/result"
  fi
  rm -rf -- "$TMP/backup"
done
export FAIL_AT=
bash "$SCRIPT" --config "$TMP/config.json"
test "$(stat -c %a "$TMP/backup")" = 700
test -L "$TMP/backup/latest.tar.zst.gpg"
LATEST=$(readlink "$TMP/backup/latest.tar.zst.gpg")
(cd "$TMP/backup" && sha256sum -c "$LATEST.sha256")
mkdir "$TMP/extracted"
tar -xf "$TMP/backup/$LATEST" -C "$TMP/extracted"
(cd "$TMP/extracted" && sha256sum -c CONTENTS.sha256)
exec 9>"$TMP/backup.lock"
flock -n 9
: > "$CALLS"
reject --config "$TMP/config.json"
! grep -q 'stop ' "$CALLS"
flock -u 9
echo "full-backup tests passed"
