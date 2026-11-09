# Backup and Restore

Verified against: aa62233b

This document describes how ShinGo Edge backups work, how to enable automatic backups, and how to restore a failed edge machine onto replacement hardware.

The procedures below are written for operators and engineers who may need to recover a station months or years after the system was installed.

## Overview

ShinGo Edge can create full, compressed station snapshots and store them in an S3-compatible object store.

Each backup is a single `.tar.gz` archive of three entries (`backup.DBEntryName`, `backup.ConfigEntryName`,
`backup.ManifestName`):

- shingoedge.db, the SQLite database
- shingoedge.yaml, the config
- manifest.json, the station ID, creation time, app version and each file's size and SHA-256 (`backup.Manifest`)

The backup represents the full local edge state at the time it was created. A restore replaces the local Edge config and SQLite database with the contents of a selected backup.

## What A Backup Covers

A backup includes:

- Edge configuration
- Processes, styles and their claims, flows and recipes, nodes, operator screens, reporting points, shifts
- Local order state
- Local counters, order history and other persisted edge state in the SQLite database

This is intended to support full machine replacement after an edge computer failure.

## Backup Storage Requirements

ShinGo Edge expects an S3-compatible object store.

Examples:

- `rust-fs`
- MinIO
- AWS S3
- other S3-compatible systems

The edge does not require ShinGo Core application involvement in the backup process. It only needs network access to the configured object storage endpoint.

## Enabling Automatic Backups

Log into the Edge web UI, open `/config` and scroll to the **Backups** section. Until storage is set up, click
**Set up storage…** to show its fields:

- **Endpoint**: the S3-compatible endpoint URL
- **Bucket and region**
- **Keys**: the access key and the secret key. A blank secret keeps the saved one; **Remove** clears it.
- **Path-style addresses**: most storage that is not Amazon's needs this on
- **Skip the certificate check**: only for storage with a self-signed certificate
- **Back up automatically**, and how often (**every**)
- **How many to keep**: hourly, daily, weekly, monthly

Recommended procedure:

1. Enter the storage settings.
2. Click **Test connection**. It tests what is typed, before anything is saved.
3. Confirm the note reads "The storage answered."
4. Turn on **Back up automatically** and set the interval.
5. Click **Save** on the page's save bar.
6. Click **Back up now** to create an initial known-good backup.

Important notes:

- Automatic backups should not be enabled until **Test connection** succeeds.
- **Back up now** works with automatic backups off.
- Backup listings are scoped to the current station ID.

## Manual Backup

From the **Backups** section on `/config`:

1. Click **Back up now**.
2. Wait for the note to read "Backed up."
3. Under **Restore from a backup**, click **Show backups** and confirm the new backup is listed.

Use a manual backup:

- after commissioning a station
- after major setup changes
- before planned hardware replacement
- before maintenance that may put the edge at risk

## Automatic Backup Behavior

When automatic backups are enabled, ShinGo Edge creates backups:

- on the configured schedule interval
- after successful setup/configuration changes, debounced so a burst of edits makes one backup
  (`(*Service).RequestBackup`)

Only one backup runs at a time.

The **Backups** title on `/config` carries the live words, in plant time:

- `automatic` or `off`
- `last HH:MM`, or `no backup yet`
- `next HH:MM` while automatic backups are on
- `last run failed` when the latest run failed
- the warning tone when automatic backups are behind schedule or the last run failed

`off · no storage set up` means no storage is configured. A backup staged for restore reads "pending restart"
in the backup list.

## Retention

Retention is managed by ShinGo Edge using time-bucket rules. After each backup, the newest backup is always
kept, plus the newest backup in each of the most recent hourly, daily, weekly and monthly buckets, up to each
bucket's count; the rest are deleted (`backup.retainedKeys`).

Defaults (the `backup.keep_*` keys in the [README](../README.md) key table):

- `48` hourly
- `14` daily
- `8` weekly
- `12` monthly

This keeps recent rollback points while bounding storage growth.

## Preventing Wrong-Station Restore

ShinGo Edge includes multiple safeguards:

- backup listing is restricted to the current station ID prefix
- each backup contains a manifest with the station ID
- restore validation rejects a backup whose manifest station ID does not match the expected station
- web restore asks for the station ID before staging: the box opens holding this station's ID, and anything
  else typed, or Cancel, stops the restore
- CLI restore requires the station ID to be typed again before restore begins

Do not bypass these checks casually. A cross-station restore can apply the wrong configuration, orders, and inventory state to a machine.

## Restore From The Web UI

This is appropriate when the edge machine is still available and you intentionally want to roll back.

Procedure:

1. Open `/config`.
2. Scroll to **Restore from a backup** (shown once storage is set up) and click **Show backups**.
3. Review the backups listed for the station.
4. Click **Restore on restart** on the desired backup.
5. Confirm the station ID when prompted.
6. Restart ShinGo Edge.

Important:

- The restore is staged first and applied on next startup.
- The running process is not hot-swapped while online.
- **Staging writes next to the config file**, not to a state directory of its own: `backup.StageRestoreArchive` puts `pending-restore.tar.gz` in `<dir of config>/.shingoedge-backup/`. On a deployed station the config lives under `/etc/shingo/`, which is root-owned, so the Edge process cannot create that directory and **Restore on restart** fails to stage. Use the CLI restore below on a deployed station — it runs as whoever invokes it.

## Restore A Failed Edge Machine To Replacement Hardware

This is the primary disaster-recovery procedure.

### Prerequisites

- replacement computer with ShinGo Edge installed
- network access to the backup object store
- station ID for the failed machine
- S3-compatible storage endpoint, bucket, and credentials

### Procedure

1. Install ShinGo Edge on the replacement machine.
2. Open a shell on the replacement machine.
3. Run:

```sh
./shingoedge --restore --config shingoedge.yaml
```

4. When prompted, enter:
   - `Station ID`
   - `S3 Endpoint URL`
   - `Bucket`
   - `Region`
   - `Access Key`
   - `Secret Key`
   - whether path-style S3 is required
   - whether TLS verification should be skipped
5. Wait for the built-in connection test to succeed.
6. Review the listed backups for that station.
7. Choose the correct backup.
8. Re-type the station ID to confirm restore.
9. Wait for the restore to complete.
10. ShinGo Edge will then continue normal startup using the restored config and database.

### After Startup

After the replacement machine starts successfully:

1. Log into the web UI.
2. Verify station identity and setup values.
3. Verify Kafka connectivity.
4. Verify PLC / WarLink connectivity.
5. Verify the most recent expected orders and inventory state.
6. Run a manual backup once the station is confirmed healthy.

## Expected Effects Of Restoring An Older Backup

If the latest usable backup is older than current time, all local changes after that backup timestamp are lost.

If the backup is 2 hours old, expect:

- local counts may be stale
- local order state may be behind
- hourly production counts may be incomplete
- recent operator screen edits may be missing
- recent local configuration changes may be missing

The main recovery tasks after restore are:

1. verify current line material counts
2. verify in-flight or recently completed orders
3. confirm PLC connectivity and current counter behavior
4. check for any setup changes that occurred after the backup

## Operational Recommendations

- Keep automatic backups enabled on production stations unless another backup method fully replaces them.
- Run **Back up now** after major setup changes.
- Monitor stale-backup warnings in the UI.
- Periodically test the restore workflow on a non-production machine.
- Keep a secure record of:
  - station IDs
  - storage endpoint
  - bucket names
  - credential ownership and rotation process

## Command Reference

Normal startup:

```sh
./shingoedge --config shingoedge.yaml
```

Interactive restore and then startup:

```sh
./shingoedge --restore --config shingoedge.yaml
```
