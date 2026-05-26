// Package immichapi adapts the Immich REST API for read-only audit. The
// adapter authenticates with x-api-key, paginates POST /api/search/metadata,
// and surfaces every asset the server knows about — TIMELINE, ARCHIVE,
// HIDDEN, LOCKED visibility classes, and isTrashed=true assets — so the
// cleanup-report layer owns the policy of which states count as "present".
package immichapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPageSize = 1000
	defaultTimeout  = 60 * time.Second
)

// AssetType mirrors Immich's AssetTypeEnum.
type AssetType string

const (
	TypeImage AssetType = "IMAGE"
	TypeVideo AssetType = "VIDEO"
	TypeAudio AssetType = "AUDIO"
	TypeOther AssetType = "OTHER"
)

// Visibility mirrors Immich's AssetVisibility enum. The wire values are
// lowercase ("timeline", "archive", "hidden", "locked") in Immich 2.x — the
// API reference at api.immich.app shows them in uppercase, but the running
// server returns lowercase; the constants match what the server actually
// emits so a direct == comparison works.
type Visibility string

const (
	VisibilityTimeline Visibility = "timeline"
	VisibilityArchive  Visibility = "archive"
	VisibilityHidden   Visibility = "hidden"
	VisibilityLocked   Visibility = "locked"
)

// Asset is the adapter-boundary view of an Immich asset. The bridge to the
// macOS Photos library is DeviceAssetID, which equals PHAsset.ZUUID for any
// asset uploaded via the Immich iOS app. Assets uploaded by other paths
// (web import, Lightroom export, immich-go) have unrelated DeviceAssetIDs
// and aren't bridge-matchable for v1 — see SPEC.md § Assets in Immich
// without a PHAsset id.
type Asset struct {
	ID                 string // Immich UUID
	DeviceAssetID      string // PHAsset.ZUUID when uploaded via the iOS app
	DeviceID           string
	OriginalFilename   string
	OriginalPath       string // storage path inside Immich's library directory
	OriginalMimeType   string
	ChecksumSHA1Base64 string // Immich computes SHA-1 on import; stored base64-encoded
	Type               AssetType
	Visibility         Visibility
	IsTrashed          bool
	IsArchived         bool
	IsFavorite         bool
	LivePhotoVideoID   string // empty when this asset has no paired motion video
	LibraryID          string // empty when null
	FileCreatedAt      time.Time
	FileModifiedAt     time.Time
}

// Client is an authenticated handle on an Immich server.
type Client struct {
	baseURL string // e.g. https://immich.local.dewijer.nl — without trailing /api
	apiKey  string
	http    *http.Client
}

// Open builds a client. baseURL accepts the Immich server URL with or
// without a trailing /api; it is normalised internally so callers can
// supply whichever form their config uses. apiKey is the per-user API key
// from Immich's Account Settings; load it from env or keychain, never
// inline in YAML.
func Open(baseURL, apiKey string) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("immichapi: baseURL is required")
	}
	if apiKey == "" {
		return nil, errors.New("immichapi: apiKey is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("immichapi: parse baseURL %q: %w", baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("immichapi: baseURL needs scheme and host, got %q", baseURL)
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/api")
	u.Path = p
	return &Client{
		baseURL: u.String(),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: defaultTimeout},
	}, nil
}

// Close is a no-op today — there's no connection pool to flush — but is
// kept for symmetry with adapters that hold real resources.
func (c *Client) Close() error { return nil }

// URL returns the normalised base URL for diagnostic logging.
func (c *Client) URL() string { return c.baseURL }

// Assets returns every asset the Immich server knows about. The adapter
// passes withDeleted:true so trashed assets appear too, and does NOT set a
// visibility filter so all four visibility classes surface. Visibility
// classification is the caller's responsibility — cleanup-report typically
// treats timeline and archive as present; hidden, locked, and IsTrashed=true
// as not-present.
//
// Pagination is driven by the server's nextPage field (a string-encoded
// page number, or null on the last page). The `total` field in the wrapper
// is per-page in Immich 2.x — equal to count, NOT the lifetime total — so
// it can't be used as a terminator.
func (c *Client) Assets(ctx context.Context) ([]Asset, error) {
	var all []Asset
	page := 1
	for {
		items, nextPage, err := c.searchPage(ctx, page, defaultPageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if nextPage == "" {
			break
		}
		next, err := strconv.Atoi(nextPage)
		if err != nil {
			return nil, fmt.Errorf("immichapi: nextPage was not a numeric string (got %q)", nextPage)
		}
		if next <= page {
			// Defensive: a server bug that returns the same or an earlier
			// page would otherwise loop forever.
			return nil, fmt.Errorf("immichapi: nextPage=%d did not advance past current page=%d", next, page)
		}
		page = next
	}
	return all, nil
}

func (c *Client) searchPage(ctx context.Context, page, size int) (items []Asset, nextPage string, err error) {
	body := map[string]any{
		"page":        page,
		"size":        size,
		"withDeleted": true,
		"withExif":    false,
		"withPeople":  false,
		"withStacked": true,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("immichapi: marshal search body: %w", err)
	}
	endpoint := c.baseURL + "/api/search/metadata"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, "", fmt.Errorf("immichapi: build request: %w", err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("immichapi: POST %s (page %d): %w", endpoint, page, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("immichapi: POST %s page %d: %s — %s",
			endpoint, page, resp.Status, strings.TrimSpace(string(snippet)))
	}

	var parsed struct {
		Assets struct {
			Count    int           `json:"count"`
			NextPage *string       `json:"nextPage"`
			Items    []apiAssetDto `json:"items"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, "", fmt.Errorf("immichapi: decode page %d: %w", page, err)
	}

	out := make([]Asset, 0, len(parsed.Assets.Items))
	for _, d := range parsed.Assets.Items {
		out = append(out, d.toAsset())
	}
	np := ""
	if parsed.Assets.NextPage != nil {
		np = *parsed.Assets.NextPage
	}
	return out, np, nil
}

// apiAssetDto is the wire representation. Pointer fields for fields the
// spec marks nullable so json.Decoder distinguishes "null" from "".
type apiAssetDto struct {
	ID               string    `json:"id"`
	DeviceAssetID    string    `json:"deviceAssetId"`
	DeviceID         string    `json:"deviceId"`
	OriginalFilename string    `json:"originalFileName"`
	OriginalPath     string    `json:"originalPath"`
	OriginalMimeType string    `json:"originalMimeType"`
	Checksum         string    `json:"checksum"`
	Type             string    `json:"type"`
	Visibility       string    `json:"visibility"`
	IsTrashed        bool      `json:"isTrashed"`
	IsArchived       bool      `json:"isArchived"`
	IsFavorite       bool      `json:"isFavorite"`
	LivePhotoVideoID *string   `json:"livePhotoVideoId"`
	LibraryID        *string   `json:"libraryId"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	FileModifiedAt   time.Time `json:"fileModifiedAt"`
}

func (d apiAssetDto) toAsset() Asset {
	a := Asset{
		ID:                 d.ID,
		DeviceAssetID:      d.DeviceAssetID,
		DeviceID:           d.DeviceID,
		OriginalFilename:   d.OriginalFilename,
		OriginalPath:       d.OriginalPath,
		OriginalMimeType:   d.OriginalMimeType,
		ChecksumSHA1Base64: d.Checksum,
		Type:               AssetType(d.Type),
		Visibility:         Visibility(d.Visibility),
		IsTrashed:          d.IsTrashed,
		IsArchived:         d.IsArchived,
		IsFavorite:         d.IsFavorite,
		FileCreatedAt:      d.FileCreatedAt,
		FileModifiedAt:     d.FileModifiedAt,
	}
	if d.LivePhotoVideoID != nil {
		a.LivePhotoVideoID = *d.LivePhotoVideoID
	}
	if d.LibraryID != nil {
		a.LibraryID = *d.LibraryID
	}
	return a
}
