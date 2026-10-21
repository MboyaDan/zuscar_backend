#!/usr/bin/env bash
# Backs up the Zuscar database to one encrypted file, and (optionally) proves the backup restores.
#
#   DATABASE_URL        required  the database to back up (Supabase session pooler string)
#   BACKUP_PASSPHRASE   required  encrypts the file; without it the file cannot be read, so keep it in a password manager
#   OUT_DIR             optional  where the file goes (default ./backup-out)
#   RESTORE_URL         optional  a SCRATCH database to restore into and compare. It is emptied first: never point it at live data.
#
# Needs pg_dump, pg_restore, psql (version 15 or newer, at least the server's) and gpg.
set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
: "${BACKUP_PASSPHRASE:?BACKUP_PASSPHRASE is required}"
OUT_DIR="${OUT_DIR:-./backup-out}"
stamp="$(date -u +%Y%m%d-%H%M%SZ)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$OUT_DIR"

echo "Dumping the public schema..."
pg_dump --format=custom --schema=public --no-owner --no-privileges --file="$work/zuscar.dump" "$DATABASE_URL"
entries="$(pg_restore --list "$work/zuscar.dump" | grep -vc '^;')"
if [ "$entries" -lt 50 ]; then
  echo "The dump has only $entries entries, far fewer than expected. Not keeping it." >&2
  exit 1
fi
echo "Dump written ($entries entries, $(du -h "$work/zuscar.dump" | cut -f1))."

if [ -n "${RESTORE_URL:-}" ]; then
  echo "Restore check into the scratch database..."
  psql "$RESTORE_URL" -v ON_ERROR_STOP=1 -q -c "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public; CREATE EXTENSION IF NOT EXISTS btree_gist;"
  # The scratch database already has an empty public schema and the extension, so leave the dump's own CREATE SCHEMA out.
  pg_restore --list "$work/zuscar.dump" | grep -v ' SCHEMA - public ' > "$work/restore.list"
  pg_restore --no-owner --no-privileges --exit-on-error --use-list="$work/restore.list" --dbname="$RESTORE_URL" "$work/zuscar.dump"
  q="SELECT (SELECT count(*) FROM information_schema.tables WHERE table_schema='public')
          || ',' || (SELECT count(*) FROM schema_migrations)
          || ',' || (SELECT count(*) FROM users)
          || ',' || (SELECT count(*) FROM sales)
          || ',' || (SELECT count(*) FROM audit_logs)"
  a="$(psql "$DATABASE_URL" -Atc "$q")"
  b="$(psql "$RESTORE_URL" -Atc "$q")"
  if [ "$a" != "$b" ]; then
    echo "RESTORE CHECK FAILED: live has [tables,migrations,users,sales,audit]=$a but the restore has $b" >&2
    exit 1
  fi
  echo "Restore check passed: tables,migrations,users,sales,audit = $a"
fi

out="$OUT_DIR/zuscar-$stamp.dump.gpg"
printf '%s' "$BACKUP_PASSPHRASE" | gpg --batch --yes --pinentry-mode loopback --passphrase-fd 0 --symmetric --cipher-algo AES256 --output "$out" "$work/zuscar.dump"
echo "Encrypted backup: $out ($(du -h "$out" | cut -f1))"
