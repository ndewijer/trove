# Trove — Spec

> A read-only librarian that audits whether each photo is durably present across the user's existing backup chain, so photos can be deleted from Photos.app/iCloud with confidence.

## Architecture pivot (2026-05-26)

The v1 design was originally **Mac Photos.sqlite-centric**: trove would read the user's Mac Photos library and bridge to Immich via PHAsset identifier and filename heuristics. Validation against the user's real library disproved three load-bearing assumptions:

1. **`PHAsset.localIdentifier` is per-device, not iCloud-synced.** Same iCloud asset gets a different UUID on Mac vs iPhone. The UUID-prefix bridge from Mac to Immich (which stores iPhone-side identifiers) yields zero matches.
2. **`Photos.sqlite.ZFILENAME` is the *internal storage* filename (`<UUID>.heic`),** not the original. Even the corrected `ZADDITIONALASSETATTRIBUTES.ZORIGINALFILENAME` only overlaps 16% with the iOS app's view of filenames for the same library.
3. **Mac's `ZORIGINALSTABLEHASH` is an Apple-internal content identifier,** not SHA-1; doesn't match Immich's `checksum` directly.

Validation against the user's library showed the **only bridge that works at scale is via the Immich iOS app's exported SQLite** — its `local_asset_entity.checksum` (base64 SHA-1) equi-joins exactly with `remote_asset_entity.checksum` (also base64 SHA-1) inside the export. Ground truth: **21,120 PHAsset → Immich-asset matches** on a 30,584-asset library, indistinguishable from the iOS app's own dedup pass.

**v1 is therefore phone-DB-centric**:
- Source of truth for "what's in the iPhone library and in scope": Immich iOS app SQLite export (manual, semi-annual)
- Source of truth for "what's already in Immich": same export's `remote_asset_entity`, plus the live Immich REST API for `originalPath`
- Bridge: checksum equi-join inside the phone DB. No filename heuristics, no UUID inference.
- The `photos_macos` adapter (commits 28b6f32 → 39039fe) is **shelved** — code stays in git history, marked deferred. Mac-only assets (~1,357 in the reference library) are out of v1 scope.

See `docs/immich-iphone-export.md` for the schema and `docs/photos-sqlite.md` for the corrected Mac-side reference.

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

- **Single Go binary** running on the user's Mac. The Mac is the host, not the data source — the bulk of the audit reads from the Immich iOS app's exported SQLite (manually dropped in by the user) plus the live Immich REST API plus remote replicas.
- **Phone DB export** at a configured path (default: `~/Library/Application Support/trove/data/immich_export_*.sqlite`). User exports periodically from the Immich iOS app.
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

The matching primitive between tiers is **byte hash (SHA-1)** — promoted from the original "exact identifier" design after validation showed PHAsset.localIdentifier is per-device and cross-device identifier matching from Mac to Immich yields zero coverage.

| From | To | Bridge |
|---|---|---|
| iOS Photos (via phone DB) | Immich | `local_asset_entity.checksum` (base64 SHA-1, computed by the iOS app at import) equi-joins with `remote_asset_entity.checksum` (also base64 SHA-1). Exact equality. |
| Immich | zfs snapshot | Storage path — Immich's library lives at `cache/immich/library/...`; the snapshot has the same relative path |
| Immich | S3 | S3 key — derived from the storage path and the `s3_backup` script's source/prefix |

The phone-DB SHA-1 is computed on the iPhone, the Immich SHA-1 is computed on the server at upload — they match because they hash the same bytes. The "Immich uses SHA-1, trove deepcheck uses SHA-256" mismatch noted in earlier drafts is real but doesn't affect this bridge: the SHA-1 → SHA-1 equality holds end-to-end on the bridge axis. SHA-256 is reserved for `trove deepcheck` which verifies replicas against each other independently.

**iCloud-optimised assets** (PHAsset present but original bytes not on device) have `local_asset_entity.checksum IS NULL` and cannot be bridged via this path. Default v1 verdict: indeterminate, never safe-to-delete. Fallbacks (filename+size match, download originals + re-export) are deferred extensions.

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

Rewritten around the phone-DB-centric bridge.

```
1. Read phone-DB local_album_entity. The audit scope is:
     in-scope = members of any album with backup_selection = 0     (user-selected)
              − members of any album with backup_selection = 2     (user-excluded — WhatsApp, Reinvent, etc.)
   (System albums with backup_selection = 1 are never in scope.)

2. For each in-scope local_asset_entity row L:
     If L.checksum IS NULL:
       report "indeterminate — iCloud-optimised, original not on device" — not safe
       continue
     Find remote_asset_entity R where R.checksum = L.checksum (and matching owner_id):
       If none: report "missing from Immich" — not safe
       If R.deleted_at IS NOT NULL OR R.visibility ∈ {hidden, locked}: report "not present in Immich (trashed/hidden/locked)" — not safe

3. For each matched R:
     Query Immich REST API for originalPath (the phone DB doesn't carry it)
     Verify originalPath exists in cache/immich snapshot S where (now - S.timestamp) ≥ settling[snapshot]
     Verify derived S3 key exists in S3 where (now - object.last_modified) ≥ settling[s3]
   If all replicas verified:
     L is "safe to delete from Photos.app"
   Else:
     L is "not yet durable" with the missing replicas named
```

Notes vs the earlier design:
- **No multi-resource enumeration step.** The phone DB tracks one row per PHAsset, not per resource. Live Photo motion videos appear as separate `remote_asset_entity` rows linked via `live_photo_video_id`; if that link is intact and both halves bridge to Immich, the asset is durable. The detailed resource model from § Resource model and edge cases still applies but is enforced by the iOS app's upload logic rather than re-validated by trove.
- **Visibility policy is local to cleanup-report,** matching SPEC § Immich asset states: `timeline` and `archive` count as present; `hidden`, `locked`, and `deleted_at IS NOT NULL` count as not-present.
- **Settling tolerance** still mandatory on snapshot and S3 replicas — see CLAUDE.md hard rule #4.

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

### `immich_phone_export` (v1 primary source)

- Reads the SQLite file the Immich iOS app exports (Settings → … → Export DB). User AirDrops it to Mac periodically (semi-annual cadence acceptable for historical cleanup).
- **Returns per asset**: `id` (iOS PHAsset.localIdentifier — per-device, but stable within this DB), `name` (filename), `checksum` (base64 SHA-1, the bridge to Immich), `i_cloud_id` (iCloud-stable UUID prefix, for future Mac↔phone correlation), album memberships via `local_album_asset_entity`, backup_selection per album.
- **Album scope**: read `local_album_entity.backup_selection` directly. `0` = selected, `1` = system/not-selected, `2` = explicitly excluded. Replaces the manually-maintained exclusion list in earlier drafts.
- **Indeterminate verdict**: PHAssets with `checksum IS NULL` (iCloud-optimised, original not on device) cannot be bridged via this adapter. Default v1 behaviour: report as indeterminate, never safe-to-delete.
- **Staleness**: the export is a snapshot. Trove must warn (or refuse) if the file's mtime is older than a threshold — recommended default 30 days, hard fail at 6 months.
- See `docs/immich-iphone-export.md` for the schema reference and the validation results.

### `photos_macos` (DEFERRED — not on v1 critical path)

> Code lives in `internal/adapter/photosmacos/` (commits 28b6f32 → 39039fe) and remains buildable, but is not invoked by `trove cleanup-report`. It survives in `trove scan photos` for diagnostic use against the Mac Photos library; the matching results don't bridge to Immich.

Reason for deferral: see § Architecture pivot above. The Mac Photos.sqlite cannot bridge to Immich at scale (per-device localIdentifier, wrong filename field, Apple-internal stable hash that isn't SHA-1). Resurrect when:
- Mac-only assets (~1,357 in the reference library — imported to Mac without iCloud sync to phone) become in-scope, or
- A PhotoKit Swift helper replaces the SQLite reader and exposes `cloudIdentifier` / SHA-1 directly.

#### Preserved adapter spec for revival
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
- **Role under the v1 pivot**: provides `originalPath` per Immich asset id (the phone DB doesn't carry path), used downstream for snapshot/S3 verification. The bulk-list endpoint is also still useful for `trove scan immich` diagnostics independent of the phone-DB bridge.
- **Auth header**: `x-api-key: <key>`. Key sourced from env (default `IMMICH_API_KEY`) or keychain — never inline in config.
- **Bulk endpoint**: `POST /api/search/metadata` with body `{page, size, withDeleted: true, withStacked: true, withExif: false, withPeople: false}`. The adapter does NOT set a `visibility` filter, so timeline / archive / hidden / locked assets all surface; the caller (cleanup-report) decides which classes count as "present" (default: timeline + archive present; hidden, locked, `isTrashed: true` not-present, per § Immich asset states).
- **Pagination**: incrementing `page=1, 2, …` with `size=1000`, driven by the wrapper's `nextPage` field — a *string-encoded* page number, or `null` on the last page. The wrapper's `total` and `count` are per-page values in Immich 2.x (verified against 2.7.5, not the lifetime total the api.immich.app reference suggested), so `nextPage` is the only authoritative terminator. The client also fails fast if a server returns `nextPage <= currentPage`, which would otherwise spin forever.
- **Visibility wire values are lowercase** (`"timeline"`, `"archive"`, `"hidden"`, `"locked"`) — the api.immich.app reference shows them uppercase but the running server emits lowercase; the adapter's constants match the wire so direct equality works.
- **Returns per asset**: Immich UUID (`id`), `deviceAssetId` (per-uploader source identifier — see bridge note below), `deviceId`, `originalFileName`, `originalPath` (path inside Immich's `library/` directory), `originalMimeType`, `checksum` (base64-encoded SHA-1 — **not** SHA-256; recorded as `ChecksumSHA1Base64` to keep that explicit), `type` (`IMAGE`/`VIDEO`/`AUDIO`/`OTHER`), `visibility` (lowercase `timeline`/`archive`/`hidden`/`locked`), `isTrashed`/`isArchived`/`isFavorite`, `livePhotoVideoId` (empty when the asset has no paired motion), `libraryId`, `fileCreatedAt`, `fileModifiedAt`.
- **deviceAssetId is multi-uploader, not iOS-only.** Every Immich uploader populates it: the iOS app uses `<PHAsset.ZUUID>/L0/<seq>` (e.g. `AB51027D-FEAE-4F0A-A662-009BF9C2E43B/L0/001`); the Android app uses the MediaStore numeric id (e.g. `1000040840`); web/CLI imports use other shapes. The Photos → Immich bridge is therefore directional: for each Photos PHAsset ZUUID, find the Immich asset whose `deviceAssetId` matches the `<ZUUID>/…` prefix. Non-UUID-shaped `deviceAssetId` values (a partner's Android uploads, manual imports) simply don't match any Photos asset — which is correct, those assets aren't in this user's Photos library to begin with.
- **Stack children inflate the count.** The adapter sends `withStacked: true`, so stacked children appear as top-level items in pagination results. This means `total assets` will exceed the Immich web UI's count (which often shows stack parents only). Cleanup-report must verify durability per child, so this is correct — but the discrepancy is real and not a bug.
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
  iphone:
    type: immich_phone_export
    # Path to the Immich iOS app's exported SQLite. Glob accepted —
    # newest matching file wins.
    export_path: ~/Library/Application Support/trove/data/immich_export_*.sqlite
    # Warn if the export is older than this; refuse if much older.
    staleness_warn: 30d
    staleness_fail: 180d

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
    source: iphone
    # Album scope read from the phone DB's local_album_entity.backup_selection.
    # No manual include/exclude list needed — the iOS app's own policy is
    # authoritative. If the user wants to override, they can list albums
    # here that will be additionally excluded regardless of backup_selection.
    extra_exclude_albums: []
    replicas:
      - storage: immich
        bind: checksum                     # phone DB SHA-1 → Immich SHA-1
      - storage: immich_snapshot
        bind: immich_path                  # via Immich originalPath
      - storage: immich_s3
        bind: immich_path                  # via Immich originalPath
```

## Open questions for the builder

1. **iCloud-optimised assets without local checksum.** In the reference library, 8,841 PHAssets have no local SHA-1 (originals not on the device). Cleanup-report options: (a) indeterminate, never safe-to-delete (recommended default — conservative); (b) fall back to filename+size match against Immich `exifInfo.fileSizeInByte`; (c) prompt the user to download originals and re-export. Lock the choice before shipping.
2. **Phone DB staleness thresholds.** Default warn=30d, fail=180d are guesses based on the 24h backup-chain settling window. Validate against the user's real export cadence.
3. **Owner scoping.** The phone DB's `remote_asset_entity` includes shared-library and partner content (e.g. a wife's Android-uploaded photos). The bridge query should restrict matches to `owner_id == auth_user_entity.id` so cleanup-report only considers the user's own assets, not their partner's.
4. **Mac-only assets.** ~1,357 in the reference library — imported to Mac without iCloud sync to phone. Out of v1 scope. Revisit when `photos_macos` is revived.
5. **Phone DB schema versioning.** Inspected against Immich iOS app 2.7.5 (`user_version=22` in the SQLite header). The adapter should fail loudly on an unrecognised schema version rather than silently mis-reading.

## Build constraints

- Read-only against external systems.
- Idempotent. Repeated `scan`/`verify` converge.
- Credentials only via env or keychain.
- Pure Go; CGO only if a clear win. `modernc.org/sqlite` for the state DB.
- Adapter-boundary fakes are sufficient for v1 tests. End-to-end tests against real Immich/S3/Photos can wait.
- Single binary, no runtime deps.

## Layout sketch

```
cmd/trove/                       main, CLI dispatch
internal/store/                  SQLite schema + queries (state DB)
internal/model/                  Asset, Storage, Presence, Flow
internal/adapter/
  immichphoneexport/   (v1)      reads the iOS app's SQLite export — source of truth
  immichapi/           (v1)      Immich REST — originalPath + scan diagnostics
  sshzfs/              (v1)      ZFS snapshot adapter
  s3rclone/            (v1)      S3 adapter
  photosmacos/         (deferred — Mac-only assets, not on the v1 path)
internal/flow/                   flow evaluation, settling logic, cleanup-report
internal/config/                 YAML loading + validation
```

## What success looks like for v1

- The user exports the Immich iOS app SQLite, AirDrops it to Mac, drops it in the configured path.
- `trove scan iphone` reports the in-scope set (per `local_album_entity.backup_selection`), bridged-to-Immich count (checksum equi-join), and indeterminate count (iCloud-optimised). Numbers ballpark the iOS app's own "Backup / Remainder" view.
- `trove scan immich`, `trove scan immich_snapshot`, `trove scan immich_s3` report each replica's inventory.
- `trove cleanup-report` returns a list of N PHAsset identifiers proven durable across (Immich + snapshot + S3). The user spot-checks 5 with `trove deepcheck` (SHA-256 against replica bytes), confirms, and deletes them in Photos.app.
- Nothing has been moved, copied, or modified anywhere outside trove's own SQLite.
