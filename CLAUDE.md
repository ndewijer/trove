## Claude Code — Mission-Critical

Prefer explicit over implicit. Prefer boring over clever. Prefer observable over silent.

## Commands available

- `/review` — review current work against mission-critical standards
- `/challenge` — challenge assumptions and design decisions
- `/risk-check` — full blast radius and rollback analysis
- `/explain` — explain unfamiliar code, infra, or patterns
- `/pre-deploy` — quick GO/NO-GO deployment check
- `/what-if-this-fails` — trace failure cascades and recovery paths

For contributors: `/new-pack` and `/new-skill` scaffold new content.

## Skills

Default skills (always linked): `sbp-architecture-review`, `sbp-deploy-checklist`.

Run `sbp-skills list` to see all available skills. Engineers enable more with `sbp-skills enable <name>` or by copying from `skills/` to `~/.claude/skills/`.

---

# CLAUDE.md — Working on trove

## What trove is

A read-only librarian that audits whether each photo is durably present across the user's existing backup chain, so photos can be deleted from Photos.app/iCloud with confidence. **Trove never moves bytes** — Lightroom, Immich's iOS app, syncoid, rclone, and Backblaze do all the actual replication. Trove only observes.

The full design lives in `SPEC.md`. Read it before making non-trivial changes.

## Hard rules

1. **Never add a mover.** If a feature would have trove copy, sync, or replicate bytes, that's the wrong layer. Redirect: it belongs in Lightroom config, syncoid policy, the rclone user script, or Backblaze settings.
2. **Never propose deleting from a tier without verifying durability.** Cleanup recommendations must be hash- or path-verified against every configured replica, with settling tolerance respected. Don't infer "probably synced by now."
3. **The Immich iOS app's `backup_selection` is authoritative for audit scope.** The phone-DB export's `local_album_entity.backup_selection` enum (`0`=selected, `1`=system, `2`=excluded) IS the iOS app's own policy. Trove reads it directly — no parallel exclusion list to maintain manually. (The current set of `2`-flagged albums is WhatsApp, Reinvent, Animated, SBP, DJI Album; that's the iOS app's state, not a trove config.)
4. **Settling tolerance is mandatory on async replicas.** The full chain takes ~24h to settle (autosnap + syncoid + S3 sync). Don't flag drift inside that window.
5. **Read-only outside trove's own state DB.** Adapters never write to Photos.app, Immich, the Unraid box, or S3.

## Stack

- Go, single binary, runs on macOS.
- SQLite via `modernc.org/sqlite` (CGO-free).
- YAML config.
- Credentials via env or macOS keychain — never inline.

## Existing infrastructure trove must understand (not change)

- iPhone Photos = iCloud Photos. Same library; you can't keep one and drop the other.
- Immich iOS app is the iPhone → server uploader, with album-include/exclude filters configured in the app.
- Immich library: `/mnt/cache/immich/library` on `orion.local.dewijer.nl` (Unraid).
- ZFS auto-snapshot: daily/weekly/monthly snapshots of `cache/immich`.
- syncoid replicates `cache/immich` → `ten-tb/backup/cache_immich` (the second RAID5 diskset, same machine) daily ~01:15.
- A user script `s3_backup` runs rclone (single remote `s3:`) to push to versioned S3.

## Verification expectations

- For adapter changes: write a fake storage and unit-test the adapter boundary.
- For changes affecting cleanup-report semantics: don't ship without manually running `trove deepcheck` on a real asset and confirming SHA-256 matches across all replicas.
- Don't run `trove cleanup-report` against the user's real data and surface the output in chat unless they explicitly ask. The asset list is private.

## When in doubt

- Audit, don't move.
- Verify, don't infer.
- Preserve, don't trim.
- Read SPEC.md.

<!-- cage-runtime: start -->
## CAGE environment notes

- **Multi-line git commits**: rage blocks `$()` and heredocs — use `git commit -F /tmp/msg.txt` or chain `-m` flags.
- **git push**: targets the local Forgejo repo — your work is reviewed there before reaching production.
<!-- cage-runtime: end -->
