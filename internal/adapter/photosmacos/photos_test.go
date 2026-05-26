package photosmacos

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// fixture describes one synthetic Photos.sqlite, used to exercise Open() and
// the query paths. The join-table prefix is configurable so each test can
// pin a different macOS-version-flavoured Z_NNASSETS name.
type fixture struct {
	joinPrefix     int // e.g. 33 → Z_33ASSETS, Z_33ALBUMS
	assetColPrefix int // e.g.  3 → Z_3ASSETS (column inside the join table)
	assets         []fixtureAsset
	albums         []fixtureAlbum
	memberships    []fixtureMembership
	resources      []fixtureResource
}

type fixtureAsset struct {
	pk          int64
	uuid        string
	filename    string
	directory   string
	uti         string
	dateCreated float64 // CoreData epoch seconds
	playback    int
	trashed     int
	visibility  int
	hidden      int
}

type fixtureAlbum struct {
	pk      int64
	title   string
	kind    int
	trashed int
}

type fixtureMembership struct {
	albumPK int64
	assetPK int64
}

type fixtureResource struct {
	assetPK     int64
	rType       int // ZRESOURCETYPE
	subtype     int // ZDATASTORESUBTYPE
	dataLength  int64
	fingerprint string
	compactUTI  int
	trashed     int
}

func buildPhotosSQLite(t *testing.T, f fixture) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Photos.sqlite")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE ZASSET (
			Z_PK INTEGER PRIMARY KEY,
			ZUUID TEXT,
			ZFILENAME TEXT,
			ZDIRECTORY TEXT,
			ZUNIFORMTYPEIDENTIFIER TEXT,
			ZDATECREATED REAL,
			ZPLAYBACKSTYLE INTEGER,
			ZTRASHEDSTATE INTEGER,
			ZVISIBILITYSTATE INTEGER,
			ZHIDDEN INTEGER
		)`,
		`CREATE TABLE ZGENERICALBUM (
			Z_PK INTEGER PRIMARY KEY,
			ZTITLE TEXT,
			ZKIND INTEGER,
			ZTRASHEDSTATE INTEGER
		)`,
		fmt.Sprintf(`CREATE TABLE "Z_%dASSETS" (
			"Z_%dALBUMS" INTEGER,
			"Z_%dASSETS" INTEGER
		)`, f.joinPrefix, f.joinPrefix, f.assetColPrefix),
		`CREATE TABLE ZINTERNALRESOURCE (
			Z_PK INTEGER PRIMARY KEY,
			ZASSET INTEGER,
			ZRESOURCETYPE INTEGER,
			ZDATASTORESUBTYPE INTEGER,
			ZDATALENGTH INTEGER,
			ZFINGERPRINT TEXT,
			ZCOMPACTUTI INTEGER,
			ZTRASHEDSTATE INTEGER
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create schema: %v\nSQL: %s", err, s)
		}
	}

	for _, a := range f.assets {
		_, err := db.Exec(`
			INSERT INTO ZASSET
			(Z_PK, ZUUID, ZFILENAME, ZDIRECTORY, ZUNIFORMTYPEIDENTIFIER,
			 ZDATECREATED, ZPLAYBACKSTYLE, ZTRASHEDSTATE, ZVISIBILITYSTATE, ZHIDDEN)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			a.pk, a.uuid, a.filename, a.directory, a.uti,
			a.dateCreated, a.playback, a.trashed, a.visibility, a.hidden,
		)
		if err != nil {
			t.Fatalf("insert asset %s: %v", a.uuid, err)
		}
	}
	for _, al := range f.albums {
		_, err := db.Exec(`
			INSERT INTO ZGENERICALBUM (Z_PK, ZTITLE, ZKIND, ZTRASHEDSTATE)
			VALUES (?,?,?,?)`,
			al.pk, al.title, al.kind, al.trashed,
		)
		if err != nil {
			t.Fatalf("insert album %s: %v", al.title, err)
		}
	}
	for _, m := range f.memberships {
		_, err := db.Exec(fmt.Sprintf(`
			INSERT INTO "Z_%dASSETS" ("Z_%dALBUMS", "Z_%dASSETS") VALUES (?, ?)`,
			f.joinPrefix, f.joinPrefix, f.assetColPrefix,
		), m.albumPK, m.assetPK)
		if err != nil {
			t.Fatalf("insert membership: %v", err)
		}
	}
	for _, r := range f.resources {
		_, err := db.Exec(`
			INSERT INTO ZINTERNALRESOURCE
			(ZASSET, ZRESOURCETYPE, ZDATASTORESUBTYPE, ZDATALENGTH,
			 ZFINGERPRINT, ZCOMPACTUTI, ZTRASHEDSTATE)
			VALUES (?,?,?,?,?,?,?)`,
			r.assetPK, r.rType, r.subtype, r.dataLength,
			r.fingerprint, r.compactUTI, r.trashed,
		)
		if err != nil {
			t.Fatalf("insert resource (asset %d, type %d): %v", r.assetPK, r.rType, err)
		}
	}
	return path
}

// baseFixture returns a small but realistic library: a still, a Live Photo,
// a standalone video, a trashed asset, a hidden asset, and a non-visible
// asset (e.g. iCloud-shared). Three albums; the "Animated" exclusion target
// is deliberately absent — see ExcludedAssets contract.
func baseFixture(joinPrefix, assetColPrefix int) fixture {
	return fixture{
		joinPrefix:     joinPrefix,
		assetColPrefix: assetColPrefix,
		assets: []fixtureAsset{
			{pk: 1, uuid: "UUID-STILL", filename: "IMG_0001.HEIC", directory: "DCIM/100APPLE", uti: "public.heic", dateCreated: 700000000, playback: 1},
			{pk: 2, uuid: "UUID-LIVE", filename: "IMG_0002.HEIC", directory: "DCIM/100APPLE", uti: "public.heic", dateCreated: 700001000, playback: 3},
			{pk: 3, uuid: "UUID-VIDEO", filename: "IMG_0003.MOV", directory: "DCIM/100APPLE", uti: "com.apple.quicktime-movie", dateCreated: 700002000, playback: 4},
			{pk: 4, uuid: "UUID-TRASHED", filename: "IMG_0004.HEIC", uti: "public.heic", dateCreated: 700003000, playback: 1, trashed: 1},
			{pk: 5, uuid: "UUID-HIDDEN", filename: "IMG_0005.HEIC", uti: "public.heic", dateCreated: 700004000, playback: 1, hidden: 1},
			{pk: 6, uuid: "UUID-NONVISIBLE", filename: "IMG_0006.HEIC", uti: "public.heic", dateCreated: 700005000, playback: 1, visibility: 2},
			{pk: 7, uuid: "UUID-IN-WHATSAPP", filename: "IMG_0007.JPG", uti: "public.jpeg", dateCreated: 700006000, playback: 1},
			{pk: 8, uuid: "UUID-IN-SBP", filename: "IMG_0008.JPG", uti: "public.jpeg", dateCreated: 700007000, playback: 1},
		},
		albums: []fixtureAlbum{
			{pk: 100, title: "WhatsApp", kind: 2},
			{pk: 101, title: "SBP", kind: 2},
			{pk: 102, title: "Family", kind: 2},
			// Note: no "Animated" album — ExcludedAssets must tolerate this.
		},
		memberships: []fixtureMembership{
			{albumPK: 100, assetPK: 7}, // UUID-IN-WHATSAPP in WhatsApp
			{albumPK: 101, assetPK: 8}, // UUID-IN-SBP in SBP
			{albumPK: 102, assetPK: 1}, // UUID-STILL also in Family (not excluded)
		},
		resources: []fixtureResource{
			// Canonical originals — what the adapter must surface.
			{assetPK: 1, rType: 0, subtype: 1, dataLength: 1_000_000, fingerprint: "fp-still", compactUTI: 3},         // UUID-STILL: HEIC
			{assetPK: 2, rType: 0, subtype: 1, dataLength: 800_000, fingerprint: "fp-live-still", compactUTI: 3},      // UUID-LIVE: HEIC
			{assetPK: 2, rType: 3, subtype: 18, dataLength: 5_000_000, fingerprint: "fp-live-motion", compactUTI: 23}, // UUID-LIVE: motion MOV
			{assetPK: 3, rType: 1, subtype: 1, dataLength: 20_000_000, fingerprint: "fp-video", compactUTI: 23},       // UUID-VIDEO: MOV
			{assetPK: 7, rType: 0, subtype: 1, dataLength: 500_000, fingerprint: "fp-whatsapp", compactUTI: 1},        // UUID-IN-WHATSAPP: JPEG
			{assetPK: 8, rType: 0, subtype: 1, dataLength: 600_000, fingerprint: "fp-sbp", compactUTI: 1},             // UUID-IN-SBP: JPEG
			// Noise that the adapter must filter out, exercised implicitly by every Assets() test:
			{assetPK: 1, rType: 14, subtype: 5, dataLength: 50_000, compactUTI: 36},                               // thumbnail → drop
			{assetPK: 2, rType: 3, subtype: 7, dataLength: 1_500_000, compactUTI: 24},                             // compressed Live motion derivative → drop
			{assetPK: 3, rType: 1, subtype: 4, dataLength: 2_000_000, compactUTI: 1},                              // JPEG preview of the video → drop
			{assetPK: 7, rType: 0, subtype: 1, dataLength: 500_000, fingerprint: "fp-whatsapp-stale", trashed: 1}, // trashed canonical → drop
		},
	}
}

func TestOpen_MissingFile(t *testing.T) {
	_, err := Open("/no/such/path/Photos.sqlite")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "Full Disk Access") {
		// Either the FDA hint fires (most likely on macOS, where SQLite
		// returns "unable to open database file") OR we fall through to the
		// generic error. The generic error is acceptable too; the hint just
		// has to be present when applicable. Both contain a clear "open" or
		// "cannot open" phrase.
		if !strings.Contains(err.Error(), "open") {
			t.Errorf("error %q does not mention open failure", err.Error())
		}
	}
}

func TestOpen_NotPhotosSqlite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notphotos.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE foo(x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("expected error for non-Photos sqlite, got nil")
	}
	if !strings.Contains(err.Error(), "ZASSET") {
		t.Errorf("error should mention ZASSET; got: %v", err)
	}
}

func TestOpen_DiscoversJoinTable(t *testing.T) {
	// Vary the macOS-version-flavoured prefixes to prove runtime discovery.
	cases := []struct {
		joinPrefix int
		assetCol   int
	}{
		{joinPrefix: 33, assetCol: 3},
		{joinPrefix: 25, assetCol: 3},
		{joinPrefix: 43, assetCol: 7},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("Z_%dASSETS", tc.joinPrefix), func(t *testing.T) {
			path := buildPhotosSQLite(t, baseFixture(tc.joinPrefix, tc.assetCol))
			lib, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer lib.Close()

			wantTable := fmt.Sprintf("Z_%dASSETS", tc.joinPrefix)
			if lib.JoinTable() != wantTable {
				t.Errorf("JoinTable: got %q, want %q", lib.JoinTable(), wantTable)
			}
		})
	}
}

func TestAssets_FiltersAndShape(t *testing.T) {
	path := buildPhotosSQLite(t, baseFixture(33, 3))
	lib, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer lib.Close()

	assets, err := lib.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}

	// Expect: STILL, LIVE, VIDEO, IN-WHATSAPP, IN-SBP — five.
	// Excluded by the SQL: TRASHED (ZTRASHEDSTATE=1), HIDDEN (ZHIDDEN=1,
	// deliberate policy per SPEC.md § Hidden assets are out of scope),
	// NONVISIBLE (ZVISIBILITYSTATE!=0).
	gotIDs := make([]string, 0, len(assets))
	for _, a := range assets {
		gotIDs = append(gotIDs, a.PHAssetID)
	}
	sort.Strings(gotIDs)
	wantIDs := []string{"UUID-IN-SBP", "UUID-IN-WHATSAPP", "UUID-LIVE", "UUID-STILL", "UUID-VIDEO"}
	if !equal(gotIDs, wantIDs) {
		t.Errorf("asset IDs: got %v, want %v", gotIDs, wantIDs)
	}

	// Spot-check the Live Photo's shape.
	var live *Asset
	for i := range assets {
		if assets[i].PHAssetID == "UUID-LIVE" {
			live = &assets[i]
			break
		}
	}
	if live == nil {
		t.Fatal("UUID-LIVE not in results")
	}
	if live.PlaybackStyle != PlaybackLivePhoto {
		t.Errorf("UUID-LIVE PlaybackStyle: got %d, want %d", live.PlaybackStyle, PlaybackLivePhoto)
	}
	if live.Filename != "IMG_0002.HEIC" {
		t.Errorf("UUID-LIVE Filename: got %q", live.Filename)
	}
	wantDate := time.Unix(700001000+coreDataEpochOffset, 0).UTC()
	if !live.CaptureDate.Equal(wantDate) {
		t.Errorf("UUID-LIVE CaptureDate: got %v, want %v", live.CaptureDate, wantDate)
	}
}

func TestAssets_CanonicalResourcesPerAsset(t *testing.T) {
	path := buildPhotosSQLite(t, baseFixture(33, 3))
	lib, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer lib.Close()

	assets, err := lib.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}

	// Each expectation is the canonical-resource shape baseFixture seeds.
	// Noise (thumbnails, derivatives, trashed resources, non-canonical Live
	// motion subtype) MUST be excluded — these exact-match shapes lock the
	// SQL-level filter in loadCanonicalResources.
	want := map[string][]Resource{
		"UUID-STILL": {
			{Type: ResourcePhoto, DataLength: 1_000_000, UTI: "public.heic", Fingerprint: "fp-still"},
		},
		"UUID-LIVE": {
			{Type: ResourcePhoto, DataLength: 800_000, UTI: "public.heic", Fingerprint: "fp-live-still"},
			{Type: ResourceLiveMotion, DataLength: 5_000_000, UTI: "com.apple.quicktime-movie", Fingerprint: "fp-live-motion"},
		},
		"UUID-VIDEO": {
			{Type: ResourceVideo, DataLength: 20_000_000, UTI: "com.apple.quicktime-movie", Fingerprint: "fp-video"},
		},
		"UUID-IN-WHATSAPP": {
			{Type: ResourcePhoto, DataLength: 500_000, UTI: "public.jpeg", Fingerprint: "fp-whatsapp"},
		},
		"UUID-IN-SBP": {
			{Type: ResourcePhoto, DataLength: 600_000, UTI: "public.jpeg", Fingerprint: "fp-sbp"},
		},
	}
	for uuid, expected := range want {
		got := findAsset(assets, uuid)
		if got == nil {
			t.Errorf("%s: not in results", uuid)
			continue
		}
		if !equalResources(got.Resources, expected) {
			t.Errorf("%s resources mismatch:\n  got:  %+v\n  want: %+v", uuid, got.Resources, expected)
		}
	}
}

func TestAssets_RAWPlusJPEGCarriesAlternate(t *testing.T) {
	f := baseFixture(33, 3)
	f.assets = append(f.assets, fixtureAsset{
		pk: 20, uuid: "UUID-RAWJPEG", filename: "IMG_RAW.JPG",
		uti: "public.jpeg", dateCreated: 700100000, playback: 1,
	})
	f.resources = append(f.resources,
		fixtureResource{assetPK: 20, rType: 0, subtype: 1, dataLength: 4_000_000, fingerprint: "fp-rawjpeg-jpeg", compactUTI: 1},
		fixtureResource{assetPK: 20, rType: 13, subtype: 1, dataLength: 40_000_000, fingerprint: "fp-rawjpeg-raw", compactUTI: 7},
	)
	path := buildPhotosSQLite(t, f)
	lib, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer lib.Close()

	assets, err := lib.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	a := findAsset(assets, "UUID-RAWJPEG")
	if a == nil {
		t.Fatal("UUID-RAWJPEG not found")
	}
	want := []Resource{
		{Type: ResourcePhoto, DataLength: 4_000_000, UTI: "public.jpeg", Fingerprint: "fp-rawjpeg-jpeg"},
		{Type: ResourceAlternatePhoto, DataLength: 40_000_000, UTI: "", Fingerprint: "fp-rawjpeg-raw"}, // compactUTI=7 is unmapped
	}
	if !equalResources(a.Resources, want) {
		t.Errorf("UUID-RAWJPEG resources mismatch:\n  got:  %+v\n  want: %+v", a.Resources, want)
	}
}

func TestAssets_ICloudOptimizedHasEmptyResources(t *testing.T) {
	// Asset present but no canonical original locally — only a thumbnail
	// (the iCloud Photos "Optimise Storage" state). Adapter must return
	// the asset with Resources == nil so cleanup-report can treat it as
	// not-yet-durable rather than silently dropping it.
	f := baseFixture(33, 3)
	f.assets = append(f.assets, fixtureAsset{
		pk: 30, uuid: "UUID-ICLOUD-OPT", filename: "IMG_REMOTE.HEIC",
		uti: "public.heic", dateCreated: 700200000, playback: 1,
	})
	f.resources = append(f.resources, fixtureResource{
		assetPK: 30, rType: 14, subtype: 5, dataLength: 30_000, compactUTI: 36, // thumbnail only
	})
	path := buildPhotosSQLite(t, f)
	lib, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer lib.Close()

	assets, err := lib.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	a := findAsset(assets, "UUID-ICLOUD-OPT")
	if a == nil {
		t.Fatal("UUID-ICLOUD-OPT was filtered out — adapter must surface iCloud-optimised assets so cleanup-report can mark them not-yet-durable")
	}
	if len(a.Resources) != 0 {
		t.Errorf("UUID-ICLOUD-OPT should have zero canonical resources, got %d: %+v", len(a.Resources), a.Resources)
	}
}

func TestResolveCompactUTI(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"int64 1 → jpeg", int64(1), "public.jpeg"},
		{"int64 3 → heic", int64(3), "public.heic"},
		{"int64 6 → mpeg-4", int64(6), "public.mpeg-4"},
		{"int64 23 → quicktime", int64(23), "com.apple.quicktime-movie"},
		{"int64 7 (documented unknown) → empty", int64(7), ""},
		{"int64 unmapped → empty", int64(999), ""},
		{"int (driver-narrowed) 3 → heic", int(3), "public.heic"},
		{"int (driver-narrowed) 23 → quicktime", int(23), "com.apple.quicktime-movie"},
		{"int unmapped → empty", int(999), ""},
		{"nil → empty", nil, ""},
		{"string underscore-prefixed extended UTI", "_org.webmproject.webp", "org.webmproject.webp"},
		{"string bare UTI passes through", "public.heic", "public.heic"},
		{"[]byte underscore-prefixed extended UTI", []byte("_public.jpeg"), "public.jpeg"},
		{"unknown type → empty", 1.5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCompactUTI(tc.in); got != tc.want {
				t.Errorf("resolveCompactUTI(%v %T) = %q, want %q", tc.in, tc.in, got, tc.want)
			}
		})
	}
}

func TestExcludedAssets_FoundAndMissingAlbums(t *testing.T) {
	path := buildPhotosSQLite(t, baseFixture(33, 3))
	lib, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer lib.Close()

	// "Animated" is deliberately absent in the fixture — must not error.
	excluded, err := lib.ExcludedAssets(context.Background(),
		[]string{"WhatsApp", "SBP", "Animated", "DJI Album"})
	if err != nil {
		t.Fatalf("ExcludedAssets: %v", err)
	}

	want := map[string]struct{}{
		"UUID-IN-WHATSAPP": {},
		"UUID-IN-SBP":      {},
	}
	if len(excluded) != len(want) {
		t.Errorf("excluded count: got %d, want %d (got set: %v)", len(excluded), len(want), keys(excluded))
	}
	for id := range want {
		if _, ok := excluded[id]; !ok {
			t.Errorf("missing %q in excluded set", id)
		}
	}
}

func TestExcludedAssets_EmptyList(t *testing.T) {
	path := buildPhotosSQLite(t, baseFixture(33, 3))
	lib, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer lib.Close()

	excluded, err := lib.ExcludedAssets(context.Background(), nil)
	if err != nil {
		t.Fatalf("ExcludedAssets(nil): %v", err)
	}
	if len(excluded) != 0 {
		t.Errorf("expected empty set, got %v", keys(excluded))
	}
}

func TestExcludedAssets_TrashedAssetExcludedFromResult(t *testing.T) {
	f := baseFixture(33, 3)
	// Add a trashed asset that's a member of WhatsApp — should not appear
	// in ExcludedAssets results (cleanup-report shouldn't reason about
	// already-trashed assets).
	f.assets = append(f.assets, fixtureAsset{
		pk: 9, uuid: "UUID-WHATSAPP-TRASHED", filename: "IMG_0009.JPG",
		uti: "public.jpeg", dateCreated: 700008000, playback: 1, trashed: 1,
	})
	f.memberships = append(f.memberships, fixtureMembership{albumPK: 100, assetPK: 9})

	path := buildPhotosSQLite(t, f)
	lib, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer lib.Close()

	excluded, err := lib.ExcludedAssets(context.Background(), []string{"WhatsApp"})
	if err != nil {
		t.Fatalf("ExcludedAssets: %v", err)
	}
	if _, ok := excluded["UUID-WHATSAPP-TRASHED"]; ok {
		t.Error("trashed WhatsApp asset must not appear in ExcludedAssets")
	}
	if _, ok := excluded["UUID-IN-WHATSAPP"]; !ok {
		t.Error("active WhatsApp asset missing from ExcludedAssets")
	}
}

func TestWrapError(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{"FDA: cannot open file", "unable to open database file", "Full Disk Access"},
		{"FDA: authorization denied", "authorization denied", "Full Disk Access"},
		{"Photos.app lock: database is locked", "database is locked", "Photos.app may be open"},
		{"Photos.app lock: SQLITE_BUSY", "SQLITE_BUSY: cannot start transaction", "Photos.app may be open"},
		{"generic falls through with op + path", "syntax error near 'FOO'", "query assets /tmp/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := errors.New(tc.raw)
			got := wrapError("/tmp/x", "query assets", raw)
			if !strings.Contains(got.Error(), tc.wantSub) {
				t.Errorf("wrapError(%q): got %q, want substring %q", tc.raw, got.Error(), tc.wantSub)
			}
			if !errors.Is(got, raw) {
				t.Errorf("wrapped error must preserve the underlying error via errors.Is; got chain: %v", got)
			}
		})
	}
}

func TestCoreDataDate_UnixConversion(t *testing.T) {
	// 0 CoreData seconds == 2001-01-01 00:00:00 UTC.
	got := coreDataDate(sql.NullFloat64{Float64: 0, Valid: true})
	want := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("coreDataDate(0) = %v, want %v", got, want)
	}
	// NULL maps to zero time.
	got = coreDataDate(sql.NullFloat64{Valid: false})
	if !got.IsZero() {
		t.Errorf("coreDataDate(NULL) = %v, want zero", got)
	}
}

func findAsset(assets []Asset, uuid string) *Asset {
	for i := range assets {
		if assets[i].PHAssetID == uuid {
			return &assets[i]
		}
	}
	return nil
}

func equalResources(got, want []Resource) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
