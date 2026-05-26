# Photos.sqlite — Schema reference for the `photos_macos` adapter

Source: direct inspection of a live `Photos.sqlite` dump (macOS 15, ~30k asset
library, 16,496 Live Photos confirmed).

---

## Access requirements

### Full Disk Access (TCC)

macOS TCC blocks any process from reading `Photos Library.photoslibrary/` unless
the **calling application** has Full Disk Access. This applies even to root —
`sudo` does not bypass TCC.

To grant access:

1. **System Settings → Privacy & Security → Full Disk Access**
2. Add your terminal emulator (Terminal.app, iTerm2, Warp, etc.)
3. Restart the terminal session

The same requirement applies at runtime: the `trove` binary must be run from a
terminal with Full Disk Access, or trove itself must be added to the FDA list.
The adapter should surface a clear error (`SQLITE_CANTOPEN` / `SQLITE_AUTH`)
rather than a generic "file not found."

### Database path

```
~/Pictures/Photos Library.photoslibrary/database/Photos.sqlite
```

Three files must be treated as a unit:

| File | Purpose |
|------|---------|
| `Photos.sqlite` | Main database |
| `Photos.sqlite-wal` | Write-ahead log (may contain uncommitted data) |
| `Photos.sqlite-shm` | Shared memory index for WAL |

Open with `mode=ro` (not `immutable=1`) — the WAL can mutate while Photos.app
is running. On `SQLITE_BUSY`, retry briefly, then surface: *"Photos.app appears
to be open — please close it and retry."* Do **not** copy the file as a
workaround; a live-WAL copy risks an inconsistent read.

---

## Key tables

### `ZASSET` — one row per photo/video

| Column | Type | Notes |
|--------|------|-------|
| `ZUUID` | VARCHAR | **PHAsset identifier** — primary bridge to Immich |
| `ZFILENAME` | VARCHAR | Original filename (e.g. `IMG_0042.HEIC`) |
| `ZDIRECTORY` | VARCHAR | Relative directory within the library |
| `ZUNIFORMTYPEIDENTIFIER` | VARCHAR | UTI string (`public.heic`, `public.jpeg`, `com.apple.quicktime-movie`) |
| `ZDATECREATED` | TIMESTAMP | CoreData epoch — seconds since Jan 1 2001 UTC (not Unix epoch) |
| `ZADDEDDATE` | TIMESTAMP | Same epoch |
| `ZTRASHEDSTATE` | INTEGER | `0` = active, `1` = in Trash |
| `ZVISIBILITYSTATE` | INTEGER | `0` = visible |
| `ZHIDDEN` | INTEGER | `1` = in Hidden album |
| `ZKIND` | INTEGER | `0` = photo, `1` = video |
| `ZPLAYBACKSTYLE` | INTEGER | Distinguishes Live Photos from stills — see below |
| `ZWIDTH` / `ZHEIGHT` | INTEGER | Dimensions of original |
| `ZDURATION` | FLOAT | Duration in seconds (videos) |
| `Z_PK` | INTEGER | Internal row ID — FK target for join tables |

Active, non-trashed assets: `ZTRASHEDSTATE = 0 AND ZVISIBILITYSTATE = 0`.

The `photos_macos` adapter additionally filters `ZHIDDEN = 0` — see SPEC.md § Hidden assets are out of scope for the rationale. The query sketch at the bottom of this doc reflects that filter.

#### `ZPLAYBACKSTYLE` values

| Value | Meaning |
|-------|---------|
| 1 | Regular still photo |
| 2 | Animated (GIF, APNG) |
| 3 | **Live Photo** — has a paired motion video resource |
| 4 | Video |
| 5 | Slow-motion video |

In the reference library: 12,373 regular stills, **16,496 Live Photos**, 1,842 videos.

#### CoreData date conversion

```
unix_timestamp = ZDATECREATED + 978307200
```

978307200 = seconds between Unix epoch (Jan 1 1970) and CoreData epoch (Jan 1 2001).

---

### `ZINTERNALRESOURCE` — one row per file resource within an asset

A single `ZASSET` can have many resource rows: original file, cloud derivatives,
thumbnails, edit base, and — for Live Photos — the paired motion video.

| Column | Type | Notes |
|--------|------|-------|
| `ZASSET` | INTEGER | FK → `ZASSET.Z_PK` |
| `ZRESOURCETYPE` | INTEGER | Resource class — see table below |
| `ZDATASTORESUBTYPE` | INTEGER | Storage variant — see table below |
| `ZCOMPACTUTI` | INTEGER | Compact numeric UTI index — see table below |
| `ZDATALENGTH` | INTEGER | File size in bytes |
| `ZFINGERPRINT` | VARCHAR | Photos' internal fingerprint (base64) — **not SHA-256** |
| `ZSTABLEHASH` | VARCHAR | Stable hash (base64, may be empty) |
| `ZVERSION` | INTEGER | `0` = base, `>0` = edited/derived |
| `ZTRASHEDSTATE` | INTEGER | `0` = active |

#### `ZRESOURCETYPE` values observed

| Value | Count (30k lib) | Meaning |
|-------|----------------|---------|
| 0 | 114k | Photo (HEIC/JPEG) — original + derivatives |
| 1 | 7k | Video (MOV/MP4) — standalone videos |
| 3 | 52k | **Live Photo motion video** (paired video) |
| 5 | 247 | Adjustment data |
| 6 | 536 | Adjustment base photo |
| 7 | 20 | Adjustment base video |
| 13 | 1.4k | Alternate photo (RAW DNG — only for RAW+JPEG pairs) |
| 14 | 31k | Thumbnails |

The 52k `ZRESOURCETYPE=3` rows are almost entirely the Live Photo motion videos
(~16,500 assets × 3 derivative subtypes each = ~49,500 rows). Regular still
photos have no `ZRESOURCETYPE=3` rows.

#### `ZDATASTORESUBTYPE` values

| Value | Meaning |
|-------|---------|
| 0 | In-progress / placeholder |
| 1 | **Original file** (applies to stills and videos; use this to identify the canonical source) |
| 4 | JPEG preview |
| 5 | Small thumbnail variant |
| 6 | Live Photo motion — compressed high-res (MOV, H.265) |
| 7 | Live Photo motion — compressed low-res (MOV, H.265) |
| 18 | Live Photo motion — full-quality original (largest size) |
| 19 | Live Photo motion — high-quality alternate (rare) |

**Note**: Live Photo motion video resources (`ZRESOURCETYPE=3`) never use
`ZDATASTORESUBTYPE=1`. The full-quality original motion file is subtype `18`;
subtypes `6` and `7` are compressed derivatives always created in pairs.

#### Identifying resources for durability checking

**Still photo original:**
```sql
ZRESOURCETYPE = 0 AND ZDATASTORESUBTYPE = 1 AND ZTRASHEDSTATE = 0
```

**Standalone video original:**
```sql
ZRESOURCETYPE = 1 AND ZDATASTORESUBTYPE = 1 AND ZTRASHEDSTATE = 0
```

**Live Photo motion video (full quality):**
```sql
ZRESOURCETYPE = 3 AND ZDATASTORESUBTYPE = 18 AND ZTRASHEDSTATE = 0
```

**RAW alternate (for RAW+JPEG pairs):**
```sql
ZRESOURCETYPE = 13 AND ZDATASTORESUBTYPE = 1 AND ZTRASHEDSTATE = 0
```

To check whether an asset IS a Live Photo before querying for its motion video,
use `ZASSET.ZPLAYBACKSTYLE = 3` — it is more reliable than probing for
`ZRESOURCETYPE=3` rows.

#### `ZCOMPACTUTI` values observed

| Value | UTI |
|-------|-----|
| 1 | `public.jpeg` |
| 3 | `public.heic` |
| 6 | `public.mpeg-4` |
| 7 | Unknown (rare) |
| 23 | `com.apple.quicktime-movie` (MOV) — used for both video originals and Live Photo motion |
| 24 | MOV derivative format (appears on compressed video variants) |
| 36 | Thumbnail format |
| 37 | Thumbnail variant |

These are an internal Photos enum, not the full UTI string. For the canonical
format string use `ZASSET.ZUNIFORMTYPEIDENTIFIER`.

**`ZCOMPACTUTI` is dynamically typed.** For builtin Photos formats it stores
a small integer enum (the table above). For extended/imported formats (e.g.
WebP from a third-party app), Photos stores the full UTI as a `TEXT` value
with a leading underscore, like `_org.webmproject.webp`. Readers must accept
both shapes: switch on the Go driver value (`int64` → look up; `string` /
`[]byte` → trim the leading underscore and use as the UTI). The adapter's
`resolveCompactUTI` does this dispatch.

#### `ZFINGERPRINT` is not SHA-256

`ZFINGERPRINT` is Photos' own internal fingerprint (base64-encoded, ~28 chars).
It is useful as a cheap change-detection signal but must not be compared against
Immich's `checksum` field. Deep-check SHA-256 must be computed by downloading
the actual bytes from the replica.

---

### `ZGENERICALBUM` — albums and folders

| Column | Type | Notes |
|--------|------|-------|
| `Z_PK` | INTEGER | Internal ID |
| `ZTITLE` | VARCHAR | Human-readable album name |
| `ZKIND` | INTEGER | `2` = regular user album; `3571`/`3572`/`3573` = system sync albums |
| `ZTRASHEDSTATE` | INTEGER | `0` = active |

Albums confirmed in reference library matching the Immich exclusion list:
`WhatsApp`, `DJI Album`, `Reinvent`, `SBP`. `Animated` was absent — likely only
materialises when animated GIFs are present in the library.

---

### `Z_33ASSETS` — album ↔ asset join

```sql
Z_33ALBUMS  INTEGER  →  ZGENERICALBUM.Z_PK
Z_3ASSETS   INTEGER  →  ZASSET.Z_PK
```

Example — fetch all assets in a named album:

```sql
SELECT a.ZUUID
FROM ZASSET a
JOIN Z_33ASSETS j ON j.Z_3ASSETS = a.Z_PK
JOIN ZGENERICALBUM ga ON ga.Z_PK = j.Z_33ALBUMS
WHERE ga.ZTITLE = 'WhatsApp'
  AND a.ZTRASHEDSTATE = 0;
```

**Warning**: the `Z_33ASSETS` table name is schema-version-dependent. Apple
changes the numeric prefix (`Z_29`, `Z_30`, `Z_32`, `Z_33`, …) between macOS
releases. The adapter must discover the correct name at runtime:

```sql
SELECT name FROM sqlite_master
WHERE type = 'table' AND name GLOB 'Z_*ASSETS';
```

Pick the table whose columns include a `Z_*ALBUMS` FK and a `Z_*ASSETS` FK
pointing at `ZASSET`.

---

## Cleanup-report query sketch

```sql
-- Active, visible, non-hidden assets not in any excluded album
SELECT a.ZUUID, a.ZFILENAME, a.ZDATECREATED, a.ZPLAYBACKSTYLE
FROM ZASSET a
WHERE a.ZTRASHEDSTATE = 0
  AND a.ZVISIBILITYSTATE = 0
  AND a.ZHIDDEN = 0
  AND a.ZUUID NOT IN (
    SELECT a2.ZUUID
    FROM ZASSET a2
    JOIN Z_33ASSETS j ON j.Z_3ASSETS = a2.Z_PK
    JOIN ZGENERICALBUM ga ON ga.Z_PK = j.Z_33ALBUMS
    WHERE ga.ZTITLE IN ('WhatsApp', 'Reinvent', 'Animated', 'SBP', 'DJI Album')
  );
```

For each row returned, check `ZPLAYBACKSTYLE`:
- `1` or `2` → verify still resource is durable
- `3` → verify still **and** motion video (`ZRESOURCETYPE=3, ZDATASTORESUBTYPE=18`) are both durable
- `4` or `5` → verify video resource is durable

---

## Schema stability note

Photos.sqlite is an undocumented CoreData store. Apple changes it across macOS
versions — column additions are common, join-table prefix changes (`Z_NN`) occur
across major releases. The adapter must be implemented behind an interface so a
PhotoKit-based helper can replace the SQLite path if direct reading becomes
unmaintainable. See `SPEC.md § photos_macos adapter`.
