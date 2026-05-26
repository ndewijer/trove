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

// Asset represents one PHAsset (one photo or video) at the adapter boundary.
// Resources are not populated by this slice — that lands in a follow-up
// commit so cleanup-report can verify Live motion / RAW alternate / video
// originals independently.
type Asset struct {
	PHAssetID     string // ZUUID — the bridge identifier to Immich
	Filename      string
	Directory     string
	UTI           string
	CaptureDate   time.Time
	PlaybackStyle PlaybackStyle
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
// non-hidden assets. The ZHIDDEN = 0 filter is deliberate policy — see
// SPEC.md § Hidden assets are out of scope. Resources are not populated in
// this slice — see Asset doc.
func (l *Library) Assets(ctx context.Context) ([]Asset, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT ZUUID, ZFILENAME, ZDIRECTORY, ZUNIFORMTYPEIDENTIFIER,
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
	var out []Asset
	for rows.Next() {
		var (
			uuid, filename, directory, uti sql.NullString
			dateCreated                    sql.NullFloat64
			playback                       sql.NullInt64
		)
		if err := rows.Scan(&uuid, &filename, &directory, &uti, &dateCreated, &playback); err != nil {
			return nil, wrapError(l.path, "scan asset", err)
		}
		out = append(out, Asset{
			PHAssetID:     uuid.String,
			Filename:      filename.String,
			Directory:     directory.String,
			UTI:           uti.String,
			CaptureDate:   coreDataDate(dateCreated),
			PlaybackStyle: PlaybackStyle(playback.Int64),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, wrapError(l.path, "iterate assets", err)
	}
	return out, nil
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
