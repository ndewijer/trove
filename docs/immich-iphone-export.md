# Immich iPhone export — schema reference for the `immich_phone_export` adapter

The Immich iOS app provides a user-facing "Export DB" action (Settings → … → Export) that writes a single SQLite file containing the app's complete local state: the iOS Photos assets it sees, the Immich-side assets the app knows about, the local-album backup-selection policy, plus exif, stacks, people, memories, etc.

This file is **the source of truth for trove v1's PHAsset ↔ Immich-asset bridge**. See SPEC.md § Storage adapters → `immich_phone_export`.

The local copy used during design lives at `immichdb/immich_export_*.sqlite` (gitignored). The schema below was reverse-engineered from a live export on Immich iOS app **2.7.5** against a ~30k-asset library.

---

## How the user obtains it

1. iPhone → Immich app → Settings → (Export / Debug area — exact name varies by version) → an `immich_export_<unix-ms>.sqlite` file is written
2. AirDrop the file to Mac
3. Drop it into trove's configured export directory

File size for the reference library: ~105 MB.

---

## Tables relevant to trove

The full schema has ~25 tables; trove only needs four of them. Everything else (memories, person/face, asset_face, stacks for grouping, EXIF, user metadata, partner sharing, etc.) is currently out of scope.

### `local_asset_entity` — every PHAsset on the iPhone (Immich's view)

Mirrors what the iOS app sees in iOS Photos via PhotoKit. Has one row per PHAsset, not per resource.

| Column | Type | Notes |
|--------|------|-------|
| `id` | TEXT (PK) | iOS PHAsset.localIdentifier, formatted `<UUID>/L0/001`. **Per-device** — the same iCloud asset gets a different `id` on Mac/iPad/iPhone. |
| `name` | TEXT | Original filename as the iOS app sees it (e.g. `IMG_4517.JPG`, `26872D44-…-EEF5.JPG`). |
| `type` | INTEGER | Asset type enum. |
| `checksum` | TEXT NULL | **Base64-encoded SHA-1 of the original bytes**, computed by the iOS app at import time. The bridge to `remote_asset_entity.checksum`. NULL when the original isn't on the device (iCloud "Optimise iPhone Storage"). |
| `i_cloud_id` | TEXT NULL | iCloud-stable identifier, structured as `<iCloud-UUID>:001:<base64-suffix>`. The `<iCloud-UUID>` prefix matches `ZASSET.ZCLOUDASSETGUID` on the Mac side (when the asset is iCloud-synced). |
| `created_at`, `updated_at` | TEXT | UTC ISO 8601 |
| `width`, `height`, `duration_in_seconds` | INTEGER NULL | Dimensions / duration |
| `is_favorite`, `orientation`, `playback_style` | INTEGER | |
| `adjustment_time`, `latitude`, `longitude` | (various NULL-able) | Edit / location metadata |

Indexes:
- `idx_local_asset_checksum (checksum)` — designed for the bridge
- `idx_local_asset_cloud_id (i_cloud_id)`

In the reference library: 30,584 rows; 21,743 have `checksum`; 29,227 have `i_cloud_id`.

### `remote_asset_entity` — every Immich-side asset the app knows about

| Column | Type | Notes |
|--------|------|-------|
| `id` | TEXT (PK) | Immich asset UUID. |
| `name` | TEXT | Original filename on Immich. |
| `type` | INTEGER | |
| `checksum` | TEXT NOT NULL | **Base64-encoded SHA-1** — the bridge target for `local_asset_entity.checksum`. |
| `owner_id` | TEXT | FK → `user_entity.id` |
| `visibility` | INTEGER | Lowercase wire enum (see immichapi spec) |
| `live_photo_video_id` | TEXT NULL | Live Photo pairing |
| `stack_id` | TEXT NULL | |
| `library_id` | TEXT NULL | |
| `local_date_time`, `created_at`, `updated_at`, `deleted_at` | TEXT | |
| `width`, `height`, `duration_in_seconds`, `is_favorite`, `is_edited`, `thumb_hash` | (various) | |

Indexes:
- `idx_remote_asset_checksum (checksum)`
- `idx_remote_asset_owner_checksum (owner_id, checksum)`
- `UQ_remote_assets_owner_checksum (owner_id, checksum) WHERE library_id IS NULL`

**Notable omissions vs the REST API**: no `deviceAssetId`, no `originalPath`. The phone DB doesn't carry these — they have to come from the live Immich REST API (`POST /api/search/metadata`). For trove, that's fine: bridge resolution happens via checksum here; `originalPath` is only needed downstream for snapshot/S3 path verification.

In the reference library: 56,392 rows. (Higher than the REST API's 37,330 — likely because this includes shared-library and partner content the search endpoint filters out by default.)

### `local_album_entity` — iOS Photos albums with the backup-selection policy

| Column | Type | Notes |
|--------|------|-------|
| `id` | TEXT (PK) | |
| `name` | TEXT | Album display name |
| `backup_selection` | INTEGER NOT NULL | **The iOS app's per-album backup policy** — see enum below |
| `linked_remote_album_id` | TEXT NULL | FK → `remote_album_entity.id` when this album mirrors an Immich-side album |
| `is_ios_shared_album` | INTEGER | |
| `updated_at` | TEXT | |

**`backup_selection` enum** (inferred empirically from the reference library):

| Value | Meaning |
|-------|---------|
| `0` | **Selected for backup** — user-curated albums the iOS app uploads. In the reference library: `Plantje!`, `Panoramas`, `Cinematic`, `Bursts`, `Time-lapse`, `Slo-mo`, `Tuin aanleg`, etc. |
| `1` | **System / not selected** — system-curated albums the user hasn't opted into. In the reference library: `Recents`, `Hidden`, `Screenshots`, `Selfies`, `Live Photos`, `Spatial`, `Canon EOS 6D`, etc. |
| `2` | **Explicitly excluded** — the user-maintained exclusion list. In the reference library: `WhatsApp`, `Reinvent`, `Animated`, `SBP`, `DJI Album` (the exact set captured in CLAUDE.md hard rule #3). |

In the reference library: 40 albums total — 20×`0`, 15×`1`, 5×`2`.

### `local_album_asset_entity` — many-to-many between local albums and local assets

| Column | Type | Notes |
|--------|------|-------|
| `asset_id` | TEXT | FK → `local_asset_entity.id` |
| `album_id` | TEXT | FK → `local_album_entity.id` |
| `marker` | INTEGER NULL | |

In the reference library: 73,822 rows (one PHAsset can be in many albums).

---

## The bridge model

For each in-scope PHAsset, the trove bridge resolves to an Immich asset via a single SQL equi-join inside the phone DB:

```sql
SELECT r.id AS immich_asset_id, l.id AS phasset_id, l.name, l.checksum
FROM local_asset_entity l
JOIN remote_asset_entity r ON l.checksum = r.checksum
WHERE l.checksum IS NOT NULL
  AND l.id IN (
    -- in-scope PHAssets: members of any backup_selection = 0 album,
    -- minus members of any backup_selection = 2 album
    SELECT laa.asset_id
    FROM local_album_asset_entity laa
    JOIN local_album_entity la ON la.id = laa.album_id
    WHERE la.backup_selection = 0
  );
```

**Results on the reference library**: 21,120 ground-truth bridge matches against the live Immich API's `checksum` field. Indistinguishable from running the iOS app's own "Remainder: 0" pass — the algorithm is identical.

### Indeterminate assets

PHAssets where `local_asset_entity.checksum IS NULL` are in iCloud-optimised state — the original bytes aren't on the device, so the iOS app couldn't compute a hash. In the reference library, 8,841 such assets. Cleanup-report's options:

- (a) Mark as **indeterminate**, never safe-to-delete (recommended default — conservative).
- (b) Fall back to filename + size match against Immich (size from Immich `exifInfo.fileSizeInByte`). Less reliable; collisions possible.
- (c) Prompt the user to "Download originals" in iOS Photos for those assets, then re-export the phone DB.

---

## Caveats

- **Staleness.** The phone DB is a snapshot. Recent iPhone uploads / Immich changes won't appear until re-exported. Trove should warn if the export's `created_at` is significantly older than the backup chain's settling window (~24h).
- **Other-user content.** Shared-library and partner content lands in `remote_asset_entity` with a different `owner_id`. The query above doesn't filter — trove may want to restrict matches to `owner_id == auth_user_entity.id` to scope to "your own" Immich assets.
- **`local_asset_entity.id` is per-device.** Don't confuse with Mac Photos.sqlite `ZUUID` — they don't match. See `docs/photos-sqlite.md` for the Mac side and SPEC.md § Identity & matching for why.
- **Schema version.** Inspected against export from Immich iOS app **2.7.5** (`user_version=22` in SQLite). Future versions may rename columns or restructure tables; the adapter should fail loudly on a schema it doesn't recognise.

---

## What the user does, step by step

1. iPhone → Immich app → (Settings / Debug / Storage area — locate the export action; varies by version)
2. Confirm export — file written to phone storage
3. iOS Files app → share → AirDrop → Mac
4. Drop file in trove's configured export directory (path TBD in config schema)
5. Run `trove cleanup-report` (or `trove scan immich_phone_export` for diagnostics)

The user has confirmed they'll do this **once or twice a year** for historical cleanup. Workflow ceremony tolerable at that cadence.
