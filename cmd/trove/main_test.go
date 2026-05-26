package main

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRun(t *testing.T) {
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
			args:          []string{"scan", "immich"},
			wantExit:      1,
			wantStderrSub: "trove scan immich: not implemented yet",
		},
		{
			name:          "scan photos with bad --library exits 1 with adapter error",
			args:          []string{"scan", "--library", "/no/such/Photos.sqlite", "photos"},
			wantExit:      1,
			wantStderrSub: "trove scan photos:",
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
