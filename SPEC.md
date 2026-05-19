# Trove — Spec

> A read-only librarian that audits whether each photo is durably present across the user's existing backup chain, so photos can be deleted from Photos.app/iCloud with confidence.

## Origin

In 2006 the user lost years of photos and email to a hardware failure. Email recovered (SaaS); photos didn't. Year zero for the user's photo archive is 2006.

The current pipeline (Lightroom + Immich + syncoid + rclone + Backblaze) moves bytes correctly but has no consistent cross-tier inventory. Without that, you can't safely answer "is this photo durably elsewhere?", which means you can't free space in iCloud without anxiety. Trove is that inventory layer.

## Non-goals

- **Not a mover.** Trove never copies, syncs, or replicates bytes. The existing pipeline owns movement.
- **Not a replacement** for Immich, Lightroom, syncoid, rclone, or Backblaze.
- **Not multi-user.** Single user, single machine, manual invocation.
- **Not a service.** No daemon, no web UI, no scheduling. CLI only.

## v1 scope (the only deliverable that matters)

> For which photos in the iPhone's Photos library can we prove the original exists — hash- or path-verified — in (a) Immich, (b) a recent `cache/immich` zfs snapshot, and (c) the S3 backup, such that deleting the photo from Photos.app is safe?

The output is a list of PHAsset identifiers the user can confidently delete from Photos.app to free iPhone storage and iCloud quota.

### Out of scope for v1, designed-extensible in config

- Camera library / Lightroom / `local_backup.sh` flow
- Backblaze Personal adapter (deep audits require sampled restore — separate design)
- Document libraries (My Drive, OneDrive, NickPytrik)
- Web UI, dashboards, notifications

## Architecture

- **Single Go binary** running on the user's Mac. The Mac is the only host with Photos.sqlite, the rclone S3 credentials, and (later) Backblaze visibility.
- **State**: SQLite at `~/Library/Application Support/trove/state.db`, WAL mode. CGO-free via `modernc.org/sqlite`.
- **Config**: YAML at `~/Library/Application Support/trove/config.yaml` or `--config`.
- **Credentials**: env vars or macOS keychain. Never inline in config.
- **Read-only against everything external.** The only thing trove writes is its own SQLite DB.

## Domain model

- **Asset**: one photo/video. Identity = (PHAsset identifier, optional SHA-256 of original bytes).
- **Storage**: a configured backend with a type (`photos_macos`, `immich_api`, `ssh_zfs_snapshot`, `s3_rclone`) and an identifier.
- **Presence**: a row joining (Asset, Storage) — `expected_hash`, `observed_hash`, `last_verified_at`, `verification_method` (`identity` or `deep`), `status`, opaque metadata blob (e.g., Immich asset id, S3 key, snapshot path).
- **Flow**: declarative rule. "Assets from source X, filtered by exclusions Y, must be present in replicas [A settled-after Δ_a, B settled-after Δ_b, ...]."

## Identity & matching

The matching primitive between tiers is **exact identifier**, not byte hash, wherever possible:

| From | To | Bridge |
|---|---|---|
| Photos.app | Immich | PHAsset identifier — the Immich iOS app stores it on every uploaded asset |
| Immich | zfs snapshot | Storage path — Immich's library lives at `cache/immich/library/...`; the snapshot has the same relative path |
| Immich | S3 | S3 key — derived from the storage path and the `s3_backup` script's source/prefix |

SHA-256 is used for **deep-check** (`trove deepcheck`) and for **periodic spot audits** — opt-in or sampled, not on every run. Expected hash is sourced from Immich (it computes one on import).

Perceptual hash (pHash) is **deferred**. Add it only when we hit a real "same photo, different bytes" cross-tier divergence.

## Resource model and edge cases

A PHAsset is not always one file, and "present in Immich" is not always a yes/no. The cleanup-report algorithm must respect the following.

### Multi-resource assets

A single PHAsset can hold multiple resources. Trove counts the asset as durable only when **all required resources** are present.

- **Live Photos**: still (HEIC/JPEG) + paired motion video (MOV). Immich preserves the pairing when uploaded via its iOS app; both must be durable for the PHAsset to be safe-to-delete. If Immich is missing the motion (e.g., the iOS app was configured to skip Live motion), the asset is reported as not safe — Photos is the source of truth for what resources exist; Immich must mirror them.
- **RAW + JPEG pairs**: one PHAsset with `PHAssetResourceTypePhoto` (JPEG) and `PHAssetResourceTypeAlternatePhoto` (RAW DNG). Both required.
- **Edits**: identity is the **original bytes**. Durability of the original is sufficient — the edit is reproducible from non-destructive edit metadata. Presence of the edit is reported as bonus, not required.

### Immich asset states

Treat as present when Immich-side state is `active` or `archived`. Treat as **not** present:

- `trashed` — scheduled for hard deletion. Trove must not mark these safe-to-delete from Photos.
- (Stack children count as present — they're stored normally, just grouped in the UI.)

### Photos.sqlite while Photos.app is running

Photos.sqlite uses WAL and may be locked when Photos.app is open. Open with `mode=ro` (without `immutable=1`, since the file mutates underneath). On `SQLITE_BUSY`, retry briefly; if still locked, surface a clear error asking the user to close Photos.app. Do **not** copy the file as a workaround — a live-WAL copy risks an inconsistent read.

### Assets in Immich without a PHAsset id

Immich content from before the iOS app (Lightroom export, manual web upload, etc.) has no source PHAsset identifier. For v1 these are "not bridge-matchable": a Photos.app asset will be reported "missing from Immich" even if the same bytes exist there under another upload path. SHA-256 fallback matching is a deferred extension (related to the pHash deferral noted above).

## The cleanup-report algorithm

```
For each asset A in Photos.sqlite:
  If A is in any Immich-excluded album (e.g. WhatsApp, Reinvent, Animated, SBP, DJI Album):
    skip — DELIBERATELY not in Immich; never eligible for cleanup
  If A.PHAssetID not in Immich asset list, OR the matched Immich asset is in state `trashed`:
    report: "missing from Immich" — not safe
  Enumerate Photos resources R for A (still, motion, RAW, JPEG, etc. — Photos is source of truth)
  For each resource r in R:
    Find r's counterpart in Immich (matched by resource type / pairing convention)
    If counterpart missing in Immich:
      report: "Immich missing resource <type>" — not safe; stop checking other resources
    Verify counterpart's storage path exists in cache/immich snapshot S where (now - S.timestamp) ≥ settling[snapshot]
    Verify counterpart (or its derived S3 key) exists in S3 where (now - object.last_modified) ≥ settling[s3]
  If all checks pass:
    A is "safe to delete from Photos.app"
  Else:
    A is "not yet durable" with explicit reasons
```

Default settling tolerances:
- snapshot replica: ≥ 22h (covers one autosnap + syncoid cycle at ~01:15)
- S3 replica: ≥ 30h (covers autosnap @ ~01:15 → syncoid → Unraid `cron.daily` @ 04:40 → `s3_backup` user script → rclone sync, with margin)

`trove cleanup-report` returns the safe list. The user does the actual deleting in Photos.app.

## Verification cache

Deep checks are expensive — they download bytes from S3 (the only tier without a cheap server-side hash, since the bucket uses SSE-KMS and ETag is opaque) and SHA-256 them locally. Every successful verification writes `last_verified_at`, `observed_hash`, and `verification_method` (`identity` or `deep`) to the Presence row.

`trove verify` and `trove deepcheck` skip an (Asset, Storage) pair when:
- Last verification is within the storage's freshness window (default 7d for `identity`, 90d for `deep`), AND
- Cheap identity signals (size, last-modified) match the cached values — proving the replica byte stream hasn't been rewritten.

`--force` ignores the cache. Each run reports skipped vs. re-verified counts so coverage stays auditable.

## Storage adapters (v1)

### `photos_macos`

- Reads the macOS Photos library (`~/Pictures/Photos Library.photoslibrary/database/Photos.sqlite`).
- Returns: PHAsset identifier, original filename, capture date, file size, album memberships, **resource list** (one entry per `PHAssetResource` — type such as photo/video/alternatePhoto/pairedVideo, size, UTI).
- **Implementation note**: try direct SQLite first — Photos.sqlite is well-documented (cf. `osxphotos`'s schema reverse-engineering). If Apple-version churn becomes a maintenance problem, swap in a small Swift PhotoKit helper behind the same interface.

### `immich_api`

- Talks to the Immich REST API. Read the OpenAPI spec at `https://immich.app/docs/api`.
- Auth via `IMMICH_API_KEY` env var.
- Returns: Immich asset id, source PHAsset identifier (from device-asset metadata), storage path, sha1/sha256 hash, original filename.

### `ssh_zfs_snapshot`

- SSHes to `orion.local.dewijer.nl`, picks the most recent `cache/immich@autosnap_*_daily` snapshot older than the settling tolerance, lists files within `library/` of that snapshot.
- Snapshot naming pattern: `^autosnap_\d{4}-\d{2}-\d{2}_\d{2}:\d{2}:\d{2}_(hourly|daily|weekly|monthly)$`. The adapter filters by `daily` for routine checks.
- ZFS snapshots are accessible via the hidden `.zfs/snapshot/<name>/` directory on the dataset's mountpoint, OR via `zfs list -t snapshot` + `zfs send`. The `cache/immich` dataset has `snapdir=hidden`, but `.zfs/snapshot/` is still listable as root over SSH — use it directly.
- Returns: relative path within `library/`, file size, optional hash (only when explicitly requested for deep-check; full hash sweep is too expensive for routine use).

### `s3_rclone`

- Lists S3 objects via the AWS SDK against `wijernet-prod-backups/cache_immich` in `eu-west-1` (the destination of the Unraid `s3_backup` user script, which rclone-syncs from `/mnt/ten-tb/backup/cache_immich` — the syncoid replica of live `cache/immich`).
- Auth via `AWS_*` env vars or `~/.aws/credentials`.
- Returns: object key, size, ETag, last-modified.
- **SSE-KMS caveat**: the bucket has `server_side_encryption = aws:kms`, which makes S3's ETag opaque (not MD5 of the content), even for single-part uploads. Identity-tier check uses (key + size + last-modified). Hash-tier check (`trove deepcheck`) downloads the object and SHA-256s it locally — see "Verification cache" above for how repeated work is avoided.
- **Backup chain timing**: live `cache/immich` → autosnap @ ~01:15 → syncoid (same window) → `ten-tb/backup/cache_immich` → `cron.daily` @ 04:40 → rclone sync → S3. So S3 reflects the replica state at the last rclone run, not live Immich. The 30h settling default covers this chain plus margin.
- **Key derivation**: an Immich storage path `library/<rest>` maps to S3 key `cache_immich/library/<rest>`.

## CLI surface (v1)

```
trove scan <storage>                  refresh inventory of one storage
trove scan --all                      refresh all configured storages
trove verify <flow> [--force]         check all assets in a flow against expected presences
trove cleanup-report                  THE v1 deliverable: print safe-to-delete asset list
trove deepcheck <asset-id> [--force]  pull bytes from each replica, SHA-256, compare
trove status                          summary: counts per storage, last verified, drift
```

`--force` on `verify` and `deepcheck` ignores the Verification cache and re-checks from scratch.

All commands are idempotent. `scan` and `verify` produce no false-positive churn when re-run.

## Configuration (YAML)

```yaml
storages:
  photos:
    type: photos_macos
    library_path: ~/Pictures/Photos Library.photoslibrary

  immich:
    type: immich_api
    url: https://immich.local.dewijer.nl
    api_key_env: IMMICH_API_KEY

  immich_snapshot:
    type: ssh_zfs_snapshot
    host: orion.local.dewijer.nl
    user: root
    dataset: cache/immich
    library_subpath: library
    settled_after: 22h

  immich_s3:
    type: s3_rclone
    bucket: wijernet-prod-backups
    prefix: cache_immich
    region: eu-west-1
    settled_after: 30h

flows:
  iphone_library:
    source: photos
    exclude_albums:
      # Mirrors the Immich iOS app exclude list — must stay in sync with the app
      - WhatsApp
      - Reinvent
      - Animated
      - SBP
      - DJI Album
    replicas:
      - storage: immich
        bind: phasset_id
      - storage: immich_snapshot
        bind: immich_path
      - storage: immich_s3
        bind: immich_path
```

## Open questions for the builder

1. **Photos.sqlite schema stability.** Apple changes it across macOS versions. Pin the reading code behind an interface so a PhotoKit helper can swap in if direct SQLite gets too brittle. Test against the user's current macOS first.
2. **Immich endpoint choice.** Read the OpenAPI spec; pick the cheapest endpoint that returns assets with the device-asset (PHAsset) identifier preserved. The bulk-export endpoint is probably right, but verify before committing.

## Build constraints

- Read-only against external systems.
- Idempotent. Repeated `scan`/`verify` converge.
- Credentials only via env or keychain.
- Pure Go; CGO only if a clear win. `modernc.org/sqlite` for the state DB.
- Adapter-boundary fakes are sufficient for v1 tests. End-to-end tests against real Immich/S3/Photos can wait.
- Single binary, no runtime deps.

## Layout sketch

```
cmd/trove/             main, CLI dispatch
internal/store/        SQLite schema + queries
internal/model/        Asset, Storage, Presence, Flow
internal/adapter/      one package per adapter type
  photosmacos/
  immichapi/
  sshzfs/
  s3rclone/
internal/flow/         flow evaluation, settling logic, cleanup-report
internal/config/       YAML loading + validation
```

## What success looks like for v1

- `trove scan --all` populates the state DB with everything from Photos.app, Immich, the latest settled snapshot, and S3.
- `trove verify iphone_library` reports zero unexpected drift (after exclusions are tuned to match reality).
- `trove cleanup-report` returns a list of N PHAsset identifiers. The user spot-checks 5 of them with `trove deepcheck`, sees clean SHA-256 matches across all three replicas, and feels safe deleting them in Photos.app.
- Nothing has been moved, copied, or modified anywhere outside trove's own SQLite.
