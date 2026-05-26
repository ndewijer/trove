# NEXT — pickup note (2026-05-26)

Temporary handoff doc. Delete when consumed.

## Where we landed

After a long sparring session against the user's real data, we **pivoted v1 architecture from Mac-Photos.sqlite-centric to phone-DB-centric.**

The `photos_macos` adapter (commits 28b6f32 → 39039fe) is **shelved as deferred**, not deleted. The code stays in git history for future revival; it's just not on the v1 critical path. SPEC.md § Storage adapters now reflects this.

## What broke the prior plan

The Mac Photos.sqlite turns out to be a much weaker bridge than we assumed. Three concrete findings:

1. **`PHAsset.localIdentifier` is per-device, not iCloud-synced.** Same iCloud asset gets a different UUID on Mac vs phone. Apple design, not a bug. Validated: **zero** ZUUID overlap between user's Mac Photos.sqlite and Immich iOS export, despite both having 30,584 assets in the same iCloud library.

2. **`ZFILENAME` is the *internal storage* filename (`<UUID>.heic`), not the original.** Original lives in `ZADDITIONALASSETATTRIBUTES.ZORIGINALFILENAME` — and even there, many assets have generic names like `lp_image.heic` (iOS internal name for Live Photo stills) or UUID-shaped names from web imports. Only **16% filename overlap** between Mac and phone.

3. **Mac's `ZORIGINALSTABLEHASH` is Apple's internal hash, *not* SHA-1.** Doesn't match Immich's checksum directly (Immich = base64 SHA-1; Apple = different algorithm, no `=` padding, starts with `A` prefix often).

The one cross-device-stable bridge from Mac Photos.sqlite is `ZASSET.ZCLOUDASSETGUID` → phone DB `local_asset_entity.i_cloud_id` prefix match (95.6% coverage, 29,227 / 30,584). But once we have that, we still need the phone DB to get to Immich — so the phone DB is the real source of truth, and the Mac side becomes redundant.

## What we validated against real data

Phone DB checksum equi-join: **21,120 PHAssets** durably matched to Immich assets via SHA-1 equality. This is ground truth.

For your library's upload provenance:
- 9,990 iOS-app uploads (`<UUID>/L0/001` deviceAssetId)
- ~15k immich-cli/go bulk uploads from ~5 years ago (`<filename>-<sizeInBytes>` — the suffix IS exactly the byte size, verified)
- ~22k wife's Android uploads (MediaStore numeric ids — not in your Photos library, correctly never match)
- 8,841 PHAssets without local checksum on phone (iCloud-optimised — original not on device; need fallback or "indeterminate" verdict)

## What to build next

1. **`internal/adapter/immich_phone_export`** — new adapter reading the SQLite export.
   - Inputs: `local_asset_entity`, `remote_asset_entity`, `local_album_entity`, `local_album_asset_entity`.
   - Outputs: PHAsset list filtered by `backup_selection`, with checksum-bridged Immich-asset-id when matched, indeterminate when no local checksum.
   - See `docs/immich-iphone-export.md` for the schema.
2. **Rework `internal/adapter/immichapi`** lightly: cleanup-report only needs `originalPath` per asset id now (the checksum bridge is done in the phone DB). The bulk-list endpoint we already have stays useful for `trove scan immich` diagnostics.
3. **Cleanup-report algorithm** rewritten around the new bridge — see SPEC.md § The cleanup-report algorithm.

## Open questions before coding

These came up but aren't resolved. Confirm with user before locking the design:

- **iCloud-optimised assets (8,841 of them)**: no local checksum, so the phone-DB bridge can't see them. Options: (a) treat as "indeterminate, not safe-to-delete", (b) fall back to filename+size match against Immich, (c) try matching on Apple's `ZORIGINALSTABLEHASH` against Immich-side computed hashes (if Immich exposes anything compatible — currently doesn't). Recommended default: (a).
- **Phone DB staleness**: how stale is too stale? The whole backup chain settles in ~24h, so a 24–48h-old export is fine. Warn if older than (say) 30 days? Hard-fail if older than 6 months?
- **Album scope semantics**: phone DB `backup_selection` values are `0` (selected for backup), `1` (system/not-selected), `2` (excluded by user). The user-configured `WhatsApp / Reinvent / Animated / SBP / DJI Album` exclusion list maps to `2`. Should trove honour `backup_selection` automatically, or require an explicit `include_albums` / `exclude_albums` list in `config.yaml` that the user maintains by hand? Phone DB makes manual config unnecessary, but it's a per-run dependency.
- **Mac-only assets (~1,357 of them)**: photos imported to Mac that never synced to phone. Out of scope for v1 cleanup-report (the iCloud-only premise excludes them), but worth a footnote.

## Workflow for the user (will end up in SPEC.md once locked)

Twice a year:
1. On iPhone: Immich app → Settings → some "Export" action → SQLite export file appears (this is what the user did today; the file is `immichdb/immich_export_*.sqlite`, gitignored).
2. AirDrop the file to Mac. Drop it in a folder trove knows about (TBD: `~/Library/Application Support/trove/data/` or a configured `phone_export_path`).
3. Run `trove cleanup-report`.
4. Use the printed safe-to-delete list to delete from iCloud Photos.

## Memory cues for the assistant

- The user's wife uses Android only. **Anything with `.heic` or `.mov` extension comes from him.** Captured as project memory.
- The user runs trove **semi-annually for historical cleanup**, not daily. Workflow ceremony tolerable at that cadence.
- The user enjoys **sparring/intellectual challenge** — push back on my claims, surface tradeoffs explicitly, don't agree-and-build.

## Files touched in the doc-wrap commit

- `NEXT.md` (this file)
- `SPEC.md` — architecture pivot, corrected assumptions
- `docs/photos-sqlite.md` — marked deferred, fixed wrong field documentation
- `docs/immich-iphone-export.md` (new) — schema reference for the iOS app export
- `CLAUDE.md` — hard rule #3 updated to reflect authoritative source
