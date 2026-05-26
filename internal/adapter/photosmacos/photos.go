// Package photosmacos reads the macOS Photos library via direct SQLite
// against Photos.sqlite. See docs/photos-sqlite.md for the schema reference
// and TCC/Full Disk Access requirements. Read-only against external state —
// the adapter never writes to the library.
package photosmacos

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// coreDataEpochOffset is the seconds between the Unix epoch (1970-01-01 UTC)
// and the CoreData epoch (2001-01-01 UTC). ZDATECREATED stores seconds since
// the latter. See docs/photos-sqlite.md § CoreData date conversion.
const coreDataEpochOffset = 978307200

// PlaybackStyle mirrors ZASSET.ZPLAYBACKSTYLE. See docs/photos-sqlite.md.
type PlaybackStyle int

const (
	PlaybackStill      PlaybackStyle = 1
	PlaybackAnimated   PlaybackStyle = 2
	PlaybackLivePhoto  PlaybackStyle = 3
	PlaybackVideo      PlaybackStyle = 4
	PlaybackSlowMotion PlaybackStyle = 5
)

// ResourceType identifies the four canonical original resources the adapter
// surfaces. Derivatives (thumbnails, compressed Live-motion variants, JPEG
// previews, edit-base copies) are filtered at the SQL layer because they're
// reproducible and not part of any cross-tier durability check. See SPEC.md
// § photos_macos adapter for the locked contract.
type ResourceType int

const (
	ResourcePhoto          ResourceType = 0  // still original (HEIC/JPEG), ZDATASTORESUBTYPE=1
	ResourceVideo          ResourceType = 1  // standalone video original (MOV/MP4), ZDATASTORESUBTYPE=1
	ResourceLiveMotion     ResourceType = 3  // Live Photo paired motion (MOV), ZDATASTORESUBTYPE=18
	ResourceAlternatePhoto ResourceType = 13 // RAW DNG in RAW+JPEG pairs, ZDATASTORESUBTYPE=1
)

// Resource represents one canonical original file inside a PHAsset.
type Resource struct {
	Type        ResourceType
	DataLength  int64  // ZDATALENGTH bytes (per-resource — multi-resource assets have no single size)
	UTI         string // resolved from ZCOMPACTUTI; empty when the compact code is unmapped
	Fingerprint string // ZFINGERPRINT — Photos-internal change-detection signal, NOT SHA-256
}

// Asset represents one PHAsset (one photo or video) at the adapter boundary.
// Resources is the list of canonical originals (still, video, Live motion,
// RAW alternate) — derivatives are filtered out. An asset with an empty
// Resources slice has no canonical originals materialised locally; this is
// expected when iCloud Photos "Optimise Mac Storage" has not downloaded the
// original yet. Cleanup-report must treat such assets as not-yet-durable.
type Asset struct {
	PHAssetID     string // ZUUID — the bridge identifier to Immich
	Filename      string
	Directory     string
	UTI           string
	CaptureDate   time.Time
	PlaybackStyle PlaybackStyle
	Resources     []Resource
}

// Library is an open handle on Photos.sqlite.
type Library struct {
	path        string
	db          *sql.DB
	joinTable   string // Z_NNASSETS (e.g. Z_33ASSETS); NN varies by macOS release
	albumColumn string // FK column to ZGENERICALBUM (e.g. Z_33ALBUMS)
	assetColumn string // FK column to ZASSET (e.g. Z_3ASSETS)
}

// Open opens Photos.sqlite read-only. The process must have macOS Full Disk
// Access to read inside Photos Library.photoslibrary — see
// docs/photos-sqlite.md. The file may be live; Open uses mode=ro (not
// immutable=1) so a concurrent Photos.app can keep writing.
func Open(path string) (*Library, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("photosmacos: resolve path %q: %w", path, err)
	}
	u := &url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("photosmacos: sql.Open %s: %w", abs, err)
	}
	// Photos.sqlite uses WAL and may be touched by Photos.app while we read.
	// One connection keeps the busy_timeout pragma persistent and matches the
	// adapter's single-reader use case.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 2000"); err != nil {
		db.Close()
		return nil, wrapError(abs, "set busy_timeout", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, wrapError(abs, "open", err)
	}
	if err := verifyPhotosSchema(db, abs); err != nil {
		db.Close()
		return nil, err
	}
	joinTable, albumCol, assetCol, err := discoverAlbumAssetJoinTable(db, abs)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Library{
		path:        abs,
		db:          db,
		joinTable:   joinTable,
		albumColumn: albumCol,
		assetColumn: assetCol,
	}, nil
}

// Close releases the database handle.
func (l *Library) Close() error { return l.db.Close() }

// Path returns the absolute path of the open file.
func (l *Library) Path() string { return l.path }

// JoinTable returns the discovered Z_NNASSETS table name. Exposed for
// diagnostic logging (e.g. `trove scan photos` may print it for support).
func (l *Library) JoinTable() string { return l.joinTable }

// wrapError annotates a SQLite error with an actionable hint when it matches
// a known macOS-environment failure (missing TCC grant; Photos.app holding a
// lock on the live WAL), otherwise wraps it with operation context. Used at
// every call site that can return a SQLite error so the hint fires during
// queries too, not only at Open — Photos.app commonly grabs the lock between
// Open and the first read.
func wrapError(path, op string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to open database file"),
		strings.Contains(msg, "authorization denied"):
		return fmt.Errorf(
			"photosmacos: cannot %s %s — if this is inside a Photos library, grant Full Disk Access to your terminal (System Settings → Privacy & Security → Full Disk Access). underlying error: %w",
			op, path, err,
		)
	case strings.Contains(msg, "database is locked"),
		strings.Contains(msg, "SQLITE_BUSY"):
		return fmt.Errorf(
			"photosmacos: %s appears to be locked during %s — Photos.app may be open; please close it and retry. underlying error: %w",
			path, op, err,
		)
	}
	return fmt.Errorf("photosmacos: %s %s: %w", op, path, err)
}

// verifyPhotosSchema confirms the file is a Photos.sqlite by checking for
// ZASSET. Avoids opaque "no such table" errors deeper in the call stack.
func verifyPhotosSchema(db *sql.DB, path string) error {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='ZASSET'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("photosmacos: %s does not look like Photos.sqlite (no ZASSET table)", path)
	}
	if err != nil {
		return wrapError(path, "verify schema", err)
	}
	return nil
}

// joinTablePattern matches "Z_<digits>ASSETS" — the join table linking
// ZGENERICALBUM to ZASSET. Apple changes the numeric prefix across macOS
// releases (Z_25, Z_30, Z_33 observed); runtime discovery is required.
// See docs/photos-sqlite.md § Z_33ASSETS.
var (
	joinTablePattern = regexp.MustCompile(`^Z_\d+ASSETS$`)
	albumColPattern  = regexp.MustCompile(`^Z_\d+ALBUMS$`)
	assetColPattern  = regexp.MustCompile(`^Z_\d+ASSETS$`)
)

// discoverAlbumAssetJoinTable finds the Z_NNASSETS table that joins
// ZGENERICALBUM and ZASSET. Returns the table name plus its album-FK and
// asset-FK column names. Errors when no candidate has both columns.
func discoverAlbumAssetJoinTable(db *sql.DB, path string) (table, albumCol, assetCol string, err error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name GLOB 'Z_*ASSETS'`)
	if err != nil {
		return "", "", "", wrapError(path, "enumerate Z_*ASSETS tables", err)
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "", "", "", wrapError(path, "scan Z_*ASSETS candidate", err)
		}
		if joinTablePattern.MatchString(n) {
			candidates = append(candidates, n)
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", "", wrapError(path, "iterate Z_*ASSETS candidates", err)
	}
	for _, t := range candidates {
		ac, asc, ok := columnsLookLikeAlbumAssetJoin(db, t)
		if ok {
			return t, ac, asc, nil
		}
	}
	return "", "", "", fmt.Errorf(
		"photosmacos: %s has no Z_*ASSETS join table with Z_NNALBUMS / Z_NASSETS columns (candidates: %v)",
		path, candidates,
	)
}

func columnsLookLikeAlbumAssetJoin(db *sql.DB, table string) (string, string, bool) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		return "", "", false
	}
	defer rows.Close()
	var albumCol, assetCol string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return "", "", false
		}
		if albumCol == "" && albumColPattern.MatchString(name) {
			albumCol = name
		}
		if assetCol == "" && assetColPattern.MatchString(name) {
			assetCol = name
		}
	}
	return albumCol, assetCol, albumCol != "" && assetCol != ""
}

// Assets enumerates the auditable set: active, visible, non-trashed,
// non-hidden assets, each populated with its canonical original resources.
// The ZHIDDEN = 0 filter is deliberate policy — see SPEC.md § Hidden assets
// are out of scope.
//
// Implementation: two queries — ZASSET, then a single ZINTERNALRESOURCE
// query that joins ZASSET so it inherits the same active-visible-non-hidden
// filter and a SQL-level WHERE that retains only canonical (type, subtype)
// pairs. Resources are bucketed by ZASSET.Z_PK and attached to each Asset
// in a final pass; Z_PK never leaks past the package boundary.
func (l *Library) Assets(ctx context.Context) ([]Asset, error) {
	type assetRow struct {
		pk    int64
		asset Asset
	}

	rows, err := l.db.QueryContext(ctx, `
		SELECT Z_PK, ZUUID, ZFILENAME, ZDIRECTORY, ZUNIFORMTYPEIDENTIFIER,
		       ZDATECREATED, ZPLAYBACKSTYLE
		FROM ZASSET
		WHERE ZTRASHEDSTATE = 0
		  AND ZVISIBILITYSTATE = 0
		  AND ZHIDDEN = 0
		ORDER BY Z_PK
	`)
	if err != nil {
		return nil, wrapError(l.path, "query assets", err)
	}
	defer rows.Close()
	var rowsBuf []assetRow
	for rows.Next() {
		var (
			pk                             int64
			uuid, filename, directory, uti sql.NullString
			dateCreated                    sql.NullFloat64
			playback                       sql.NullInt64
		)
		if err := rows.Scan(&pk, &uuid, &filename, &directory, &uti, &dateCreated, &playback); err != nil {
			return nil, wrapError(l.path, "scan asset", err)
		}
		rowsBuf = append(rowsBuf, assetRow{
			pk: pk,
			asset: Asset{
				PHAssetID:     uuid.String,
				Filename:      filename.String,
				Directory:     directory.String,
				UTI:           uti.String,
				CaptureDate:   coreDataDate(dateCreated),
				PlaybackStyle: PlaybackStyle(playback.Int64),
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(l.path, "iterate assets", err)
	}

	resByAsset, err := l.loadCanonicalResources(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Asset, len(rowsBuf))
	for i, ar := range rowsBuf {
		a := ar.asset
		a.Resources = resByAsset[ar.pk]
		out[i] = a
	}
	return out, nil
}

// loadCanonicalResources returns canonical originals for the auditable asset
// set (active, visible, non-hidden, non-trashed), keyed by ZASSET.Z_PK.
// The (type, subtype) pairs that count as "canonical" are the four documented
// in docs/photos-sqlite.md § Identifying resources for durability checking:
// still photo (0/1), standalone video (1/1), Live Photo motion full-quality
// (3/18), RAW alternate (13/1). Everything else — thumbnails, compressed
// Live-motion variants, JPEG previews, edit-base copies — is excluded at the
// SQL layer.
func (l *Library) loadCanonicalResources(ctx context.Context) (map[int64][]Resource, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT r.ZASSET, r.ZRESOURCETYPE, r.ZDATALENGTH, r.ZFINGERPRINT, r.ZCOMPACTUTI
		FROM ZINTERNALRESOURCE r
		JOIN ZASSET a ON a.Z_PK = r.ZASSET
		WHERE r.ZTRASHEDSTATE = 0
		  AND a.ZTRASHEDSTATE = 0
		  AND a.ZVISIBILITYSTATE = 0
		  AND a.ZHIDDEN = 0
		  AND (
		    (r.ZRESOURCETYPE = 0 AND r.ZDATASTORESUBTYPE = 1)
		    OR (r.ZRESOURCETYPE = 1 AND r.ZDATASTORESUBTYPE = 1)
		    OR (r.ZRESOURCETYPE = 3 AND r.ZDATASTORESUBTYPE = 18)
		    OR (r.ZRESOURCETYPE = 13 AND r.ZDATASTORESUBTYPE = 1)
		  )
		ORDER BY r.ZASSET, r.ZRESOURCETYPE
	`)
	if err != nil {
		return nil, wrapError(l.path, "query resources", err)
	}
	defer rows.Close()

	out := make(map[int64][]Resource)
	for rows.Next() {
		var (
			assetPK     int64
			rType       sql.NullInt64
			dataLength  sql.NullInt64
			fingerprint sql.NullString
			compactUTI  any // INTEGER for builtin Photos codes, TEXT for extended UTIs
		)
		if err := rows.Scan(&assetPK, &rType, &dataLength, &fingerprint, &compactUTI); err != nil {
			return nil, wrapError(l.path, "scan resource", err)
		}
		out[assetPK] = append(out[assetPK], Resource{
			Type:        ResourceType(rType.Int64),
			DataLength:  dataLength.Int64,
			UTI:         resolveCompactUTI(compactUTI),
			Fingerprint: fingerprint.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(l.path, "iterate resources", err)
	}
	return out, nil
}

// resolveCompactUTI converts a ZINTERNALRESOURCE.ZCOMPACTUTI cell into a UTI
// string. The column is dynamically typed: builtin Photos formats use a
// small integer enum (1=jpeg, 3=heic, 6=mpeg-4, 23=quicktime — see
// docs/photos-sqlite.md § ZCOMPACTUTI values observed), but extended UTIs
// (e.g. WebP) come through as TEXT with a leading underscore
// ("_org.webmproject.webp"). Unmapped integers and unrecognised types
// return "" so callers can distinguish "unknown" from a real UTI.
func resolveCompactUTI(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case int64:
		switch x {
		case 1:
			return "public.jpeg"
		case 3:
			return "public.heic"
		case 6:
			return "public.mpeg-4"
		case 23:
			return "com.apple.quicktime-movie"
		}
		return ""
	case string:
		return strings.TrimPrefix(x, "_")
	case []byte:
		return strings.TrimPrefix(string(x), "_")
	}
	return ""
}

// ExcludedAssets returns the PHAsset identifiers (ZUUID) of all assets that
// belong to any of the named albums. Album names that do not exist in the
// library contribute zero assets and do not produce an error — the Immich
// exclusion list is authoritative, and an album like "Animated" may not have
// materialised here yet (see docs/photos-sqlite.md § ZGENERICALBUM and the
// project hard-rule about mirroring Immich's exclusion list).
func (l *Library) ExcludedAssets(ctx context.Context, albumNames []string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if len(albumNames) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(albumNames)), ",")
	// Table and column names were validated by regex at Open time —
	// safe to interpolate. Album titles use bound parameters.
	query := fmt.Sprintf(`
		SELECT DISTINCT a.ZUUID
		FROM ZASSET a
		JOIN "%s" j ON j."%s" = a.Z_PK
		JOIN ZGENERICALBUM ga ON ga.Z_PK = j."%s"
		WHERE ga.ZTITLE IN (%s)
		  AND ga.ZTRASHEDSTATE = 0
		  AND a.ZTRASHEDSTATE = 0
	`, l.joinTable, l.assetColumn, l.albumColumn, placeholders)
	args := make([]any, len(albumNames))
	for i, n := range albumNames {
		args[i] = n
	}
	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapError(l.path, "query excluded assets", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, wrapError(l.path, "scan excluded asset", err)
		}
		out[uuid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(l.path, "iterate excluded assets", err)
	}
	return out, nil
}

func coreDataDate(f sql.NullFloat64) time.Time {
	if !f.Valid {
		return time.Time{}
	}
	secs := int64(f.Float64) + coreDataEpochOffset
	return time.Unix(secs, 0).UTC()
}
