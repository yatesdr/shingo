# Backup and Restore

This document describes how ShinGo Edge backups work, how to enable automatic backups, and how to restore a failed edge machine onto replacement hardware.

The procedures below are written for operators and engineers who may need to recover a station months or years after the system was installed.

## Overview

ShinGo Edge can create full, compressed station snapshots and store them in an S3-compatible object store.

Each backup is a single `.tar.gz` archive containing:

- `shingoedge.db`
- `shingoedge.yaml`
- `manifest.json`

The backup represents the full local edge state at the time it was created. A restore replaces the local Edge config and SQLite database with the contents of a selected backup.

## What A Backup Covers

A backup includes:

- Edge configuration
- Production lines, styles, reporting points, nodes, material slots, shifts
- Local order state
- Operator screen layouts
- Local counters, logs, and other persisted edge state in the SQLite database

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

Open the Edge web UI and go to `/setup`, then open the `Backups` section.

Configure:

- `Endpoint URL`
- `Bucket`
- `Region`
- `Access Key`
- `Secret Key`
- `Use path-style S3` if required by the storage system
- `Skip TLS verification` only if your environment requires it
- retention counts:
  - `Keep Hourly`
  - `Keep Daily`
  - `Keep Weekly`
  - `Keep Monthly`
- `Schedule Interval`

Recommended procedure:

1. Enter storage settings.
2. Click `Test Connection`.
3. Confirm the UI shows a successful connection test.
4. Enable `Automatic Backups`.
5. Click `Save Backup Settings`.
6. Click `Backup Now` to create an initial known-good backup.

Important notes:

- Automatic backups should not be enabled until `Test Connection` succeeds.
- Manual backups remain available even when automatic backups are disabled.
- Backup listings are scoped to the current station ID.

## Manual Backup

From the `Backups` section in `/setup`:

1. Click `Backup Now`.
2. Wait for the operation status to show success.
3. Confirm that a new backup appears in the backup list.

Use a manual backup:

- after commissioning a station
- after major setup changes
- before planned hardware replacement
- before maintenance that may put the edge at risk

## Automatic Backup Behavior

When automatic backups are enabled, ShinGo Edge creates backups:

- on the configured schedule interval
- after successful setup/configuration changes, using debounced triggers

Only one backup runs at a time.

The backup status area in `/setup` shows:

- whether automatic backups are enabled
- whether a backup is currently running
- last successful backup
- last failed backup
- next scheduled run
- stale backup warnings
- pending restore-on-restart status

## Retention

Retention is managed by ShinGo Edge using time-bucket rules.

Typical defaults:

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
- web restore requires the user to re-type the station ID before staging restore
- CLI restore requires station ID confirmation before restore begins

Do not bypass these checks casually. A cross-station restore can apply the wrong configuration, orders, and inventory state to a machine.

## Restore From The Web UI

This is appropriate when the edge machine is still available and you intentionally want to roll back.

Procedure:

1. Open `/setup`.
2. Open the `Backups` section.
3. Review the available backups for the station.
4. Select the desired backup.
5. Confirm the station ID when prompted.
6. Click `Restore On Restart`.
7. Restart ShinGo Edge.

Important:

- The restore is staged first and applied on next startup.
- The running process is not hot-swapped while online.

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
- anomaly confirmations/dismissals may be rolled back
- recent operator screen edits may be missing
- recent local configuration changes may be missing

The main recovery tasks after restore are:

1. verify current line material counts
2. verify in-flight or recently completed orders
3. confirm PLC connectivity and current counter behavior
4. check for any setup changes that occurred after the backup

## Operational Recommendations

- Keep automatic backups enabled on production stations unless another backup method fully replaces them.
- Run `Backup Now` after major setup changes.
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
