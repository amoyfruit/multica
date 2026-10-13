# Standalone encrypted backup

`full-backup.sh` is the only runtime script. It accepts deployment JSON supplied
by an agent and runs the complete backup in the foreground. It does not invoke
systemd, another installed backup script, or the Multica CLI.

```bash
sudo -n bash scripts/full-backup.sh --config ./backup-config.json --check
sudo -n bash scripts/full-backup.sh --config ./backup-config.json
```

`--config -` reads JSON from stdin. Configuration is mandatory; there are no
deployment defaults. `--check` is read-only: no destination creation, backup
lock, or service interruption. Actual backup briefly stops the configured
Compose services. A failed or interrupted backup attempts to start them and
verify health before exiting with failure. SIGKILL, machine failure, and a
crashed Docker daemon cannot be recovered by a shell trap.

## Agent workflow

Keep host-specific values in the Multica project description, not this repository.
Read the latest description with the authenticated `multica project get`, require
exactly one fenced `multica-backup-config` JSON block, and write that block to a
0600 file in the task workdir. Treat it as data, never shell code.

Fetch this script from the repository/ref recorded in the project description.
Use a reviewed immutable commit, not a download piped into a root shell. Record
the commit used, inspect the expected mount and free space, run `--check`, then
run the same script and JSON synchronously. A single lock in the JSON serializes
backups; do not replace an active task's configuration. Do not call a legacy
backup service or wrapper.

Remove the temporary JSON after completion. Report exit status, the
`Backup completed` filename, checksum verification and business health, as well
as storage risks. An encrypted file and valid checksum do not prove
recoverability; a separate restore exercise is required.

## Required configuration

All fields below are required; no credentials or key contents belong in the JSON.

| Fields | Meaning |
| --- | --- |
| `host` | Expected short hostname; mismatch aborts. |
| `app_dir` | Absolute application directory. |
| `compose_project` | Compose project name. |
| `compose_file`, `env_file`, `proxy_config_file` | Relative files within `app_dir` to include in the encrypted backup. |
| `destination`, `owner`, `group` | Absolute backup directory and its owner/group. A missing directory is created as 0700; symlinks and incorrect owners are rejected. |
| `storage_mount`, `storage_source` | Expected mount target and source, as reported by `findmnt -rn -T <destination-parent> -o TARGET,SOURCE`. A mismatch aborts rather than creating a directory on the wrong disk. |
| `min_free_bytes` | Positive integer minimum available space before starting. Set it to accommodate exports, an unencrypted compressed archive, and its encrypted copy; this threshold is not an exact size prediction. |
| `keep_days` | Integer 1–36500; matching old archive/checksum files are pruned using `find -mtime +keep_days` after success. |
| `postgres_container`, `backend_container` | Existing PostgreSQL container (running) and backend container for image metadata. |
| `services` | Nonempty array of Compose service names to stop and restore. |
| `uploads_volume`, `archive_image` | Existing upload volume and locally available Docker image containing `tar`/`gzip`. |
| `recipient_file`, `gpg_home` | Absolute public-key recipient file and GPG home containing the encryption public key. |
| `zstd_binary`, `docker_binary` | Absolute executable paths. |
| `health_url` | HTTP(S) endpoint that must succeed after restarting services. |
| `lock_file` | Absolute shared backup lock path; its parent must exist. |

Dependencies: root, Bash, jq, Docker with Compose v2, GnuPG, curl, tar/gzip,
zstd, util-linux (`flock`, `findmnt`), coreutils, findutils, and `getent`.
The PostgreSQL container needs `POSTGRES_USER`, `POSTGRES_DB`, `pg_dump`,
`pg_dumpall`, `psql`, and sufficient database/global-role export permissions.
The public key suffices for backup; decryption requires the corresponding
private key and any passphrase.

Each encrypted archive contains database and global-role exports, uploads,
application configuration, image/database metadata and portable relative
checksums. Output is a UTC-timestamped `.tar.zst.gpg`, its `.sha256`, and a
`latest.tar.zst.gpg` symlink. Private configuration exists in plaintext only
in the restricted temporary workdir during backup; secure storage remains an
operator responsibility.

## Isolated tests

```bash
sudo bash scripts/full-backup.test.sh
```

Tests use generated fixtures and fake Docker/GPG/curl/zstd executables; they
never stop a real service, read deployment secrets, or contact the network.
