package immichapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestOpen_Validation(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		key     string
		wantSub string
	}{
		{"empty URL", "", "k", "baseURL is required"},
		{"empty key", "https://x", "", "apiKey is required"},
		{"malformed URL", "not-a-url", "k", "scheme and host"},
		{"missing scheme", "//example.com", "k", "scheme and host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(tc.url, tc.key)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("got %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestOpen_NormalisesBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://example.com":                 "https://example.com",
		"https://example.com/":                "https://example.com",
		"https://example.com/api":             "https://example.com",
		"https://example.com/api/":            "https://example.com",
		"https://example.com:8080/api":        "https://example.com:8080",
		"http://immich.local.dewijer.nl/api/": "http://immich.local.dewijer.nl",
	}
	for in, want := range cases {
		c, err := Open(in, "k")
		if err != nil {
			t.Fatalf("Open(%q): %v", in, err)
		}
		if c.URL() != want {
			t.Errorf("URL: got %q, want %q (from %q)", c.URL(), want, in)
		}
	}
}

// fakeImmich serves a stub of POST /api/search/metadata that:
//   - Verifies x-api-key matches expectedKey
//   - Returns the requested page from `pages` (1-indexed)
//   - Sets nextPage = string(page+1) when the next page exists, null otherwise.
//     nextPage is the only authoritative termination signal in Immich 2.x —
//     `total` and `count` are per-page values and would deceive a client.
//   - Records each call's page/size/withDeleted for assertion
type fakeImmich struct {
	expectedKey string
	pages       map[int][]apiAssetDto
	calls       []recordedCall
}

type recordedCall struct {
	page        int
	size        int
	withDeleted bool
	apiKey      string
}

func (f *fakeImmich) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Page        int  `json:"page"`
			Size        int  `json:"size"`
			WithDeleted bool `json:"withDeleted"`
		}
		_ = json.Unmarshal(raw, &body)
		call := recordedCall{
			page:        body.Page,
			size:        body.Size,
			withDeleted: body.WithDeleted,
			apiKey:      r.Header.Get("x-api-key"),
		}
		f.calls = append(f.calls, call)

		if call.apiKey != f.expectedKey {
			http.Error(w, `{"message":"bad api key"}`, http.StatusUnauthorized)
			return
		}
		items := f.pages[body.Page]
		// nextPage = "<page+1>" when more pages exist, null on the last page.
		var nextPage any
		if _, hasNext := f.pages[body.Page+1]; hasNext {
			nextPage = strconv.Itoa(body.Page + 1)
		}
		out := map[string]any{
			"assets": map[string]any{
				"total":    len(items),
				"count":    len(items),
				"nextPage": nextPage,
				"items":    items,
			},
			"albums": map[string]any{
				"total":    0,
				"count":    0,
				"nextPage": nil,
				"items":    []any{},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func TestAssets_PaginatesAcrossNextPageChain(t *testing.T) {
	livePtr := func(s string) *string { return &s }
	fake := &fakeImmich{
		expectedKey: "test-key",
		pages: map[int][]apiAssetDto{
			1: {
				{ID: "id-A", DeviceAssetID: "phasset-A", OriginalFilename: "A.HEIC", OriginalPath: "library/A.HEIC", Checksum: "ck-A", Type: "IMAGE", Visibility: "timeline"},
				{ID: "id-B", DeviceAssetID: "phasset-B", OriginalFilename: "B.MOV", OriginalPath: "library/B.MOV", Checksum: "ck-B", Type: "VIDEO", Visibility: "archive"},
			},
			2: {
				{ID: "id-C", DeviceAssetID: "phasset-C", OriginalFilename: "C.HEIC", OriginalPath: "library/C.HEIC", Checksum: "ck-C", Type: "IMAGE", Visibility: "timeline", IsTrashed: true},
				{ID: "id-D", DeviceAssetID: "phasset-D", OriginalFilename: "D.HEIC", OriginalPath: "library/D.HEIC", Checksum: "ck-D", Type: "IMAGE", Visibility: "hidden", LivePhotoVideoID: livePtr("id-Dmotion")},
			},
			3: {
				{ID: "id-E", DeviceAssetID: "phasset-E", OriginalFilename: "E.HEIC", OriginalPath: "library/E.HEIC", Checksum: "ck-E", Type: "IMAGE", Visibility: "locked"},
			},
		},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, err := Open(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, err := c.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d assets, want 5: %+v", len(got), got)
	}

	// Verify the bridge field, checksum, and visibility came through intact.
	wantIDs := []string{"phasset-A", "phasset-B", "phasset-C", "phasset-D", "phasset-E"}
	for i, want := range wantIDs {
		if got[i].DeviceAssetID != want {
			t.Errorf("got[%d].DeviceAssetID = %q, want %q", i, got[i].DeviceAssetID, want)
		}
	}

	// Trashed and non-TIMELINE assets MUST be surfaced — that's the contract.
	var trashed, hidden, locked int
	for _, a := range got {
		if a.IsTrashed {
			trashed++
		}
		switch a.Visibility {
		case VisibilityHidden:
			hidden++
		case VisibilityLocked:
			locked++
		}
	}
	if trashed != 1 || hidden != 1 || locked != 1 {
		t.Errorf("visibility surface broken: trashed=%d hidden=%d locked=%d (want 1/1/1)", trashed, hidden, locked)
	}

	// Live Photo pairing field must round-trip through the nullable.
	d := got[3]
	if d.LivePhotoVideoID != "id-Dmotion" {
		t.Errorf("LivePhotoVideoID: got %q, want %q", d.LivePhotoVideoID, "id-Dmotion")
	}

	// Pagination contract: pages 1, 2, 3 requested with size=defaultPageSize,
	// withDeleted=true; auth header sent on every call.
	if len(fake.calls) != 3 {
		t.Fatalf("expected 3 pagination calls, got %d", len(fake.calls))
	}
	for i, c := range fake.calls {
		if c.page != i+1 {
			t.Errorf("call %d page: got %d, want %d", i, c.page, i+1)
		}
		if c.size != defaultPageSize {
			t.Errorf("call %d size: got %d, want %d", i, c.size, defaultPageSize)
		}
		if !c.withDeleted {
			t.Errorf("call %d withDeleted should be true (would silently drop trashed assets)", i)
		}
		if c.apiKey != "test-key" {
			t.Errorf("call %d x-api-key: got %q, want %q", i, c.apiKey, "test-key")
		}
	}
}

func TestAssets_StopsWhenNextPageIsNull(t *testing.T) {
	// Single page, no next — the only authoritative stop signal in 2.x.
	fake := &fakeImmich{
		expectedKey: "k",
		pages: map[int][]apiAssetDto{
			1: {{ID: "id-1", DeviceAssetID: "p-1", Type: "IMAGE", Visibility: "timeline"}},
		},
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c, _ := Open(srv.URL, "k")
	got, err := c.Assets(context.Background())
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d, want 1", len(got))
	}
	if len(fake.calls) != 1 {
		t.Errorf("expected exactly 1 call (page 1 with nextPage=null), got %d", len(fake.calls))
	}
}

func TestAssets_BadAPIKeyReturns401Error(t *testing.T) {
	fake := &fakeImmich{expectedKey: "right-key"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c, _ := Open(srv.URL, "wrong-key")
	_, err := c.Assets(context.Background())
	if err == nil {
		t.Fatal("expected error on bad API key, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401, got: %v", err)
	}
}

func TestAssets_RejectsNonAdvancingNextPage(t *testing.T) {
	// A buggy server returning nextPage equal to (or below) the current
	// page would otherwise spin forever. The client must fail fast.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"assets": map[string]any{
				"total":    1,
				"count":    1,
				"nextPage": "1", // never advances
				"items":    []apiAssetDto{{ID: "id-1", Visibility: "timeline", Type: "IMAGE"}},
			},
			"albums": map[string]any{"total": 0, "count": 0, "nextPage": nil, "items": []any{}},
		})
	}))
	defer srv.Close()
	c, _ := Open(srv.URL, "k")
	_, err := c.Assets(context.Background())
	if err == nil {
		t.Fatal("expected error on non-advancing nextPage, got nil")
	}
	if !strings.Contains(err.Error(), "did not advance") {
		t.Errorf("error should mention non-advancing nextPage, got: %v", err)
	}
}

func TestAssets_PropagatesContext(t *testing.T) {
	fake := &fakeImmich{expectedKey: "k", pages: map[int][]apiAssetDto{
		1: {{ID: "id-1", Type: "IMAGE", Visibility: "timeline"}},
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	c, _ := Open(srv.URL, "k")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before call
	_, err := c.Assets(ctx)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error should mention context cancellation, got: %v", err)
	}
}
