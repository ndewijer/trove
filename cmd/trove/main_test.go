package main

import (
	"bytes"
	"strings"
	"testing"
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
