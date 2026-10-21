# Backups and restoring

Supabase's free plan gives no backup you can restore from. This repository makes its own: a daily encrypted dump with an automatic restore check. When you move to Supabase Pro, keep this as a second copy.

## What runs

`.github/workflows/backup.yml` runs every day at 21:00 UTC (midnight in Nairobi) and when you press **Run workflow**. It runs `scripts/backup/backup.sh`, which:

1. Dumps the `public` schema from the production database (`pg_dump`, custom format).
2. Restores that dump into a throw-away PostgreSQL 17 container on the runner and compares the number of tables, migrations, users, sales and audit rows with the live database. A mismatch fails the run.
3. Encrypts the dump with AES-256 using `BACKUP_PASSPHRASE`.
4. Keeps the encrypted file as a workflow artifact for 30 days.

A failed run emails you from GitHub. Do not ignore that email.

## One-time setup

1. In the GitHub repository: Settings, Secrets and variables, Actions, New repository secret.
   - `PROD_DATABASE_URL`: the Supabase **session pooler** string (the same one Render uses). Not the direct one: GitHub runners are IPv4-only.
   - `BACKUP_PASSPHRASE`: a long random passphrase. Put it in your password manager now. Without it no backup can be opened.
2. Actions, backup, **Run workflow**. Read the log. It should end with "Restore check passed" and an artifact.
3. Keep the repository **private**. The file is encrypted, but it holds your business data.

## Keeping copies off GitHub

GitHub artifacts expire after 30 days and live in one account. At least once a week, download the newest artifact (Actions, the run, Artifacts) and put it on a second place you control (an external drive or a different cloud drive). Two copies, two places.

## Restore drill (do this once before go-live, then every few months)

You need `gpg`, and PostgreSQL client tools. Use a scratch database, never the live one.

```
unzip zuscar-backup-<run>.zip
gpg --output zuscar.dump --decrypt zuscar-<stamp>.dump.gpg        # asks for BACKUP_PASSPHRASE
createdb zuscar_restore
psql zuscar_restore -c "CREATE EXTENSION IF NOT EXISTS btree_gist;"
pg_restore --list zuscar.dump | grep -v ' SCHEMA - public ' > restore.list
pg_restore --no-owner --no-privileges --exit-on-error --use-list=restore.list --dbname=zuscar_restore zuscar.dump
```

The `restore.list` step leaves out the dump's own `CREATE SCHEMA public` because the scratch database already has one. Then check `SELECT count(*) FROM sales;` against what you expect and look at the newest rows in `audit_logs`.

## Restoring for real (the database is lost or damaged)

1. Make a **new** Supabase project (or empty the damaged one). Do not restore on top of data you still need.
2. Decrypt the newest good backup as above.
3. In the new project run the restore commands above against its session pooler string (replace `zuscar_restore` with the connection string).
4. Run `migrate status` from the production image against it. It should list all migrations as applied. If the backup is older than the latest release, run `migrate up`.
5. Point Render's `DATABASE_URL` at it and redeploy the API. Check `/readyz`.
6. Roles and permissions come back with the data. Everything entered after the backup time is gone: re-enter it from the paper receipts (see `outage.md`).

## What this does not cover

- Data from the last 24 hours at most can be lost (daily schedule). A manual **Run workflow** before a risky change closes that gap.
- Supabase's own internal schemas (auth, storage) are not included. Zuscar does not use them.
- GitHub can pause scheduled workflows in a repository with no activity for 60 days (public repositories). Open the Actions tab now and then.
