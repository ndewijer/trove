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

### Hidden assets are out of scope

Trove excludes hidden assets (`ZASSET.ZHIDDEN = 1`) from `Assets()` and therefore from cleanup-report. This is a deliberate over-filter — the auditor's instinct to keep everything in scope is overruled here for two operational reasons:

- The Immich iOS app does not upload hidden assets by default. An audit that includes them would mark every hidden asset "missing from Immich" — accurate but useless, since cleanup-report could never recommend a delete and the report would be cluttered with un-actionable rows.
- Photos.app's Hidden album is a *keep-but-segregate* signal from the user. Recommending deletion of hidden content — even when durable elsewhere — runs against the conservative "preserve, don't trim" default.

The filter is one SQL clause in `photos_macos`. If the user later changes the Immich iOS app to upload hidden assets, lift the filter via config; do not loosen it silently in code.

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
- **Returns per asset**: PHAsset identifier (`ZUUID`), original filename, directory, asset UTI, capture date (CoreData → Unix converted), playback style (still / animated / Live Photo / video / slow-motion), and a list of canonical-original resources.
- **Returns per resource**: type (one of `Photo`, `Video`, `LiveMotion`, `AlternatePhoto` — the four canonical originals), per-resource size (`ZDATALENGTH`, in bytes), resource UTI (resolved from `ZCOMPACTUTI`, empty when unmapped), and Photos' internal fingerprint (`ZFINGERPRINT`, useful for change-detection but **not** a SHA-256). Derivatives — thumbnails (`ZRESOURCETYPE=14`), compressed Live-motion variants (`3/6`, `3/7`), JPEG previews of videos, edit-base copies — are filtered at the SQL layer and never surface; the cleanup-report algorithm has no use for them.
- **Per-asset size is intentionally absent.** A multi-resource asset (Live Photo, RAW+JPEG) has no single "size" — callers that need a number must sum or pick per-resource.
- **Album membership is exposed as a predicate, not a list.** `ExcludedAssets(names)` returns the PHAsset-id set that belongs to any named album; full per-asset album listings are not part of the v1 surface (cleanup-report only needs the exclusion predicate).
- **iCloud-optimised assets** (asset row present, no canonical original locally) surface with `Resources == nil`. Cleanup-report treats these as not-yet-durable — never as safe-to-delete — because the bytes don't exist on disk yet.
- **macOS TCC requirement**: the process must have Full Disk Access — `sudo` does not bypass TCC. On `SQLITE_CANTOPEN` / `SQLITE_AUTH`, surface a clear message directing the user to grant FDA to their terminal. See `docs/photos-sqlite.md` for details.
- **Live Photo detection**: Live Photos are `ZASSET.ZPLAYBACKSTYLE = 3`. Their paired motion video is a separate `ZINTERNALRESOURCE` row with `ZRESOURCETYPE = 3, ZDATASTORESUBTYPE = 18`. Regular stills are `ZPLAYBACKSTYLE = 1`; standalone videos are `ZPLAYBACKSTYLE = 4`.
- **Album join table**: the join table linking albums to assets (`Z_33ASSETS` in the current schema) uses a numeric prefix that Apple changes across macOS releases. The adapter must discover it at runtime via `sqlite_master`. See `docs/photos-sqlite.md`.
- **Hidden filter**: the adapter filters `ZHIDDEN = 0` in addition to `ZTRASHEDSTATE = 0` and `ZVISIBILITYSTATE = 0`. See § Hidden assets are out of scope for the rationale.
- **Implementation note**: try direct SQLite first — schema documented in `docs/photos-sqlite.md` (reverse-engineered from a live macOS 15 library). If Apple-version churn becomes a maintenance problem, swap in a small Swift PhotoKit helper behind the same interface.

### `immich_api`

- Talks to the Immich REST API. Spec: `https://docs.immich.app/api` → published reference at `https://api.immich.app`.
- **Auth header**: `x-api-key: <key>`. Key sourced from env (default `IMMICH_API_KEY`) or keychain — never inline in config.
- **Bulk endpoint**: `POST /api/search/metadata` with body `{page, size, withDeleted: true, withStacked: true, withExif: false, withPeople: false}`. The adapter does NOT set a `visibility` filter, so timeline / archive / hidden / locked assets all surface; the caller (cleanup-report) decides which classes count as "present" (default: timeline + archive present; hidden, locked, `isTrashed: true` not-present, per § Immich asset states).
- **Pagination**: incrementing `page=1, 2, …` with `size=1000`, driven by the wrapper's `nextPage` field — a *string-encoded* page number, or `null` on the last page. The wrapper's `total` and `count` are per-page values in Immich 2.x (verified against 2.7.5, not the lifetime total the api.immich.app reference suggested), so `nextPage` is the only authoritative terminator. The client also fails fast if a server returns `nextPage <= currentPage`, which would otherwise spin forever.
- **Visibility wire values are lowercase** (`"timeline"`, `"archive"`, `"hidden"`, `"locked"`) — the api.immich.app reference shows them uppercase but the running server emits lowercase; the adapter's constants match the wire so direct equality works.
- **Returns per asset**: Immich UUID (`id`), `deviceAssetId` (PHAsset bridge — equals `ZUUID` for assets uploaded via the iOS app, unrelated for other upload paths), `deviceId`, `originalFileName`, `originalPath` (path inside Immich's `library/` directory), `originalMimeType`, `checksum` (base64-encoded SHA-1 — **not** SHA-256; recorded as `ChecksumSHA1Base64` to keep that explicit), `type` (`IMAGE`/`VIDEO`/`AUDIO`/`OTHER`), `visibility` (`TIMELINE`/`ARCHIVE`/`HIDDEN`/`LOCKED`), `isTrashed`/`isArchived`/`isFavorite`, `livePhotoVideoId` (empty when the asset has no paired motion), `libraryId`, `fileCreatedAt`, `fileModifiedAt`.
- **Checksum caveat**: Immich's `checksum` is SHA-1, while `trove deepcheck` SHA-256s downloaded bytes. The two hashes are incomparable — Immich-side identity matching uses (deviceAssetId | filename | path), not hash equality.

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
