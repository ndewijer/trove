package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRun(t *testing.T) {
	// Ensure the "scan immich without --immich-url" case is deterministic —
	// otherwise a CI/dev shell that exports IMMICH_URL would cause the
	// fallback to fire and the test would see exit 1 (auth-key-missing)
	// instead of exit 2 (URL-missing).
	t.Setenv("IMMICH_URL", "")

	tests := []struct {
		name            string
		args            []string
		wantExit        int
		wantStdoutSub   string
		wantStderrSub   string
		wantStdoutEmpty bool
		wantStderrEmpty bool
	}{
		{
			name:            "help writes usage to stdout, exits 0",
			args:            []string{"help"},
			wantExit:        0,
			wantStdoutSub:   "Usage:",
			wantStderrEmpty: true,
		},
		{
			name:            "-h writes usage to stdout, exits 0",
			args:            []string{"-h"},
			wantExit:        0,
			wantStdoutSub:   "Usage:",
			wantStderrEmpty: true,
		},
		{
			name:            "--help writes usage to stdout, exits 0",
			args:            []string{"--help"},
			wantExit:        0,
			wantStdoutSub:   "Usage:",
			wantStderrEmpty: true,
		},
		{
			name:            "no args writes usage to stderr, exits 2",
			args:            []string{},
			wantExit:        2,
			wantStderrSub:   "Usage:",
			wantStdoutEmpty: true,
		},
		{
			name:            "unknown command writes error to stderr, exits 2",
			args:            []string{"bogus"},
			wantExit:        2,
			wantStderrSub:   `unknown command "bogus"`,
			wantStdoutEmpty: true,
		},
		{
			name:          "scan with no storage exits 2",
			args:          []string{"scan"},
			wantExit:      2,
			wantStderrSub: "missing <storage>",
		},
		{
			name:          "scan --all alone exits 1 (not implemented yet)",
			args:          []string{"scan", "--all"},
			wantExit:      1,
			wantStderrSub: "not implemented yet",
		},
		{
			name:          "scan --all with positional rejects with exit 2",
			args:          []string{"scan", "--all", "photos"},
			wantExit:      2,
			wantStderrSub: "--all conflicts with positional",
		},
		{
			name:          "scan with unknown storage exits 1 (not implemented yet)",
			args:          []string{"scan", "ssh_zfs_snapshot"},
			wantExit:      1,
			wantStderrSub: "trove scan ssh_zfs_snapshot: not implemented yet",
		},
		{
			name:          "scan photos with bad --library exits 1 with adapter error",
			args:          []string{"scan", "--library", "/no/such/Photos.sqlite", "photos"},
			wantExit:      1,
			wantStderrSub: "trove scan photos:",
		},
		{
			name:          "scan immich without --immich-url exits 2",
			args:          []string{"scan", "immich"},
			wantExit:      2,
			wantStderrSub: "--immich-url is required",
		},
		{
			name:          "verify with no flow exits 2",
			args:          []string{"verify"},
			wantExit:      2,
			wantStderrSub: "missing <flow>",
		},
		{
			name:          "verify with flow exits 1 (not implemented yet)",
			args:          []string{"verify", "iphone_library"},
			wantExit:      1,
			wantStderrSub: "trove verify iphone_library: not implemented yet",
		},
		{
			name:          "verify --force passes flag parsing",
			args:          []string{"verify", "--force", "iphone_library"},
			wantExit:      1,
			wantStderrSub: "trove verify iphone_library: not implemented yet",
		},
		{
			name:          "deepcheck with no asset-id exits 2",
			args:          []string{"deepcheck"},
			wantExit:      2,
			wantStderrSub: "missing <asset-id>",
		},
		{
			name:          "deepcheck with asset-id exits 1 (not implemented yet)",
			args:          []string{"deepcheck", "ABCD-1234"},
			wantExit:      1,
			wantStderrSub: "trove deepcheck ABCD-1234: not implemented yet",
		},
		{
			name:          "cleanup-report exits 1 (not implemented yet)",
			args:          []string{"cleanup-report"},
			wantExit:      1,
			wantStderrSub: "not implemented yet",
		},
		{
			name:          "status exits 1 (not implemented yet)",
			args:          []string{"status"},
			wantExit:      1,
			wantStderrSub: "not implemented yet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			gotExit := run(tc.args, &stdout, &stderr)

			if gotExit != tc.wantExit {
				t.Errorf("exit: got %d, want %d\n  stdout: %q\n  stderr: %q",
					gotExit, tc.wantExit, stdout.String(), stderr.String())
			}
			if tc.wantStdoutSub != "" && !strings.Contains(stdout.String(), tc.wantStdoutSub) {
				t.Errorf("stdout missing substring %q\n  got: %q", tc.wantStdoutSub, stdout.String())
			}
			if tc.wantStderrSub != "" && !strings.Contains(stderr.String(), tc.wantStderrSub) {
				t.Errorf("stderr missing substring %q\n  got: %q", tc.wantStderrSub, stderr.String())
			}
			if tc.wantStdoutEmpty && stdout.Len() != 0 {
				t.Errorf("stdout should be empty, got: %q", stdout.String())
			}
			if tc.wantStderrEmpty && stderr.Len() != 0 {
				t.Errorf("stderr should be empty, got: %q", stderr.String())
			}
		})
	}
}

func TestScanPhotos_HappyPath(t *testing.T) {
	path := writeMinimalPhotosSqlite(t)

	var stdout, stderr bytes.Buffer
	exit := run([]string{"scan", "--library", path, "photos"}, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit %d; stderr=%q", exit, stderr.String())
	}
	for _, want := range []string{
		"trove scan photos",
		"library:",
		"join table:    Z_33ASSETS",
		"active assets: 2",
		"canonical resources surfaced:",
		"photo originals:        2",
		"live-motion originals:  1",
		"assets without canonical originals",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q\ngot:\n%s", want, stdout.String())
		}
	}
}

// writeMinimalPhotosSqlite builds the smallest Photos.sqlite the CLI happy
// path needs: a still + a Live Photo (with paired motion). Mirrors the
// adapter test fixture's shape — kept inline to avoid exporting test helpers
// from internal/adapter/photosmacos.
func writeMinimalPhotosSqlite(t *testing.T) string {
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
			Z_PK INTEGER PRIMARY KEY, ZUUID TEXT, ZFILENAME TEXT, ZDIRECTORY TEXT,
			ZUNIFORMTYPEIDENTIFIER TEXT, ZDATECREATED REAL, ZPLAYBACKSTYLE INTEGER,
			ZTRASHEDSTATE INTEGER, ZVISIBILITYSTATE INTEGER, ZHIDDEN INTEGER
		)`,
		`CREATE TABLE ZGENERICALBUM (Z_PK INTEGER PRIMARY KEY, ZTITLE TEXT, ZKIND INTEGER, ZTRASHEDSTATE INTEGER)`,
		`CREATE TABLE "Z_33ASSETS" ("Z_33ALBUMS" INTEGER, "Z_3ASSETS" INTEGER)`,
		`CREATE TABLE ZINTERNALRESOURCE (
			Z_PK INTEGER PRIMARY KEY, ZASSET INTEGER, ZRESOURCETYPE INTEGER,
			ZDATASTORESUBTYPE INTEGER, ZDATALENGTH INTEGER, ZFINGERPRINT TEXT,
			ZCOMPACTUTI INTEGER, ZTRASHEDSTATE INTEGER
		)`,
		// Explicit 0s on ZTRASHEDSTATE/ZVISIBILITYSTATE/ZHIDDEN — the
		// adapter's WHERE clause is `= 0`, and NULL would fail that match.
		`INSERT INTO ZASSET (Z_PK, ZUUID, ZFILENAME, ZPLAYBACKSTYLE, ZTRASHEDSTATE, ZVISIBILITYSTATE, ZHIDDEN)
		 VALUES (1, 'UUID-STILL', 'IMG_1.HEIC', 1, 0, 0, 0)`,
		`INSERT INTO ZASSET (Z_PK, ZUUID, ZFILENAME, ZPLAYBACKSTYLE, ZTRASHEDSTATE, ZVISIBILITYSTATE, ZHIDDEN)
		 VALUES (2, 'UUID-LIVE',  'IMG_2.HEIC', 3, 0, 0, 0)`,
		`INSERT INTO ZINTERNALRESOURCE (ZASSET, ZRESOURCETYPE, ZDATASTORESUBTYPE, ZDATALENGTH, ZCOMPACTUTI, ZTRASHEDSTATE)
		 VALUES (1, 0, 1, 1000000, 3, 0)`,
		`INSERT INTO ZINTERNALRESOURCE (ZASSET, ZRESOURCETYPE, ZDATASTORESUBTYPE, ZDATALENGTH, ZCOMPACTUTI, ZTRASHEDSTATE)
		 VALUES (2, 0, 1, 900000, 3, 0)`,
		`INSERT INTO ZINTERNALRESOURCE (ZASSET, ZRESOURCETYPE, ZDATASTORESUBTYPE, ZDATALENGTH, ZCOMPACTUTI, ZTRASHEDSTATE)
		 VALUES (2, 3, 18, 5000000, 23, 0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v\nSQL: %s", err, s)
		}
	}
	return path
}

func TestScanImmich_HappyPath(t *testing.T) {
	// Fake an Immich server with two pages of metadata, including a
	// trashed asset and one with a non-empty deviceAssetId.
	page1 := []map[string]any{
		// iOS-shaped deviceAssetId (UUID/L0/001) — the Photos bridge subset.
		{"id": "id-A", "deviceAssetId": "AB51027D-FEAE-4F0A-A662-009BF9C2E43B/L0/001", "originalFileName": "A.HEIC", "type": "IMAGE", "visibility": "timeline"},
		// Android-shaped (MediaStore numeric id) — not bridge-matchable from Photos.
		{"id": "id-B", "deviceAssetId": "1000040840", "originalFileName": "B.jpg", "type": "VIDEO", "visibility": "archive"},
	}
	page2 := []map[string]any{
		// No deviceAssetId — web-import or partial upload edge.
		{"id": "id-C", "deviceAssetId": "", "originalFileName": "C.HEIC", "type": "IMAGE", "visibility": "timeline", "isTrashed": true},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		var body struct {
			Page int `json:"page"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var (
			items    []map[string]any
			nextPage any
		)
		switch body.Page {
		case 1:
			items = page1
			nextPage = "2"
		case 2:
			items = page2
			nextPage = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"assets": map[string]any{
				"total":    len(items),
				"count":    len(items),
				"nextPage": nextPage,
				"items":    items,
			},
			"albums": map[string]any{"total": 0, "count": 0, "nextPage": nil, "items": []any{}},
		})
	}))
	defer srv.Close()

	t.Setenv("IMMICH_API_KEY_TEST", "test-key")

	var stdout, stderr bytes.Buffer
	exit := run([]string{"scan",
		"--immich-url", srv.URL,
		"--immich-api-key-env", "IMMICH_API_KEY_TEST",
		"immich",
	}, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit %d; stderr=%q", exit, stderr.String())
	}
	for _, want := range []string{
		"trove scan immich",
		"server:",
		"total assets:  3",
		"images:      2",
		"videos:      1",
		"timeline:    2",
		"archive:     1",
		"trashed (isTrashed=true):    1",
		"iOS-style PHAsset id:    1", // A
		"other (Android, web, …): 1", // B
		"no deviceAssetId set:    1", // C
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q\ngot:\n%s", want, stdout.String())
		}
	}
}

func TestScanImmich_URLFromEnvFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"assets": map[string]any{
				"total": 0, "count": 0, "nextPage": nil, "items": []any{},
			},
			"albums": map[string]any{"total": 0, "count": 0, "nextPage": nil, "items": []any{}},
		})
	}))
	defer srv.Close()

	t.Setenv("IMMICH_URL", srv.URL)
	t.Setenv("IMMICH_API_KEY", "k")

	var stdout, stderr bytes.Buffer
	exit := run([]string{"scan", "immich"}, &stdout, &stderr) // no --immich-url
	if exit != 0 {
		t.Fatalf("exit %d; stderr=%q", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "total assets:  0") {
		t.Errorf("expected scan to succeed via IMMICH_URL env fallback; stdout:\n%s", stdout.String())
	}
}
