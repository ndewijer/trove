// Command trove is a read-only librarian that audits whether each photo is
// durably present across the user's existing backup chain. See SPEC.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ndewijer/trove/internal/adapter/immichapi"
	"github.com/ndewijer/trove/internal/adapter/photosmacos"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "scan":
		return runScan(rest, stdout, stderr)
	case "verify":
		return runVerify(rest, stdout, stderr)
	case "cleanup-report":
		return runCleanupReport(rest, stdout, stderr)
	case "deepcheck":
		return runDeepcheck(rest, stdout, stderr)
	case "status":
		return runStatus(rest, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "trove: unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `trove — read-only audit of photo durability across backup tiers

Usage:
  trove scan <storage>                  refresh inventory of one storage
  trove scan --all                      refresh all configured storages
  trove verify <flow> [--force]         check assets in a flow against expected presences
  trove cleanup-report                  print safe-to-delete asset list (the v1 deliverable)
  trove deepcheck <asset-id> [--force]  pull bytes from each replica and SHA-256-compare
  trove status                          summary: counts per storage, last verified, drift

Global flags (per subcommand):
  --config <path>   path to config.yaml
                    (default: ~/Library/Application Support/trove/config.yaml)`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func runScan(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("scan", stderr)
	all := fs.Bool("all", false, "refresh all configured storages")
	// --config default (~/Library/Application Support/trove/config.yaml) is
	// applied where the file is opened, not during flag parsing — keeps an
	// empty flag value meaning "use default" without depending on
	// os.UserHomeDir at parse time. Same contract in every subcommand below.
	cfgPath := fs.String("config", "", "path to config.yaml")
	library := fs.String("library", "",
		"override Photos.sqlite path (only used by 'scan photos'; "+
			"default: ~/Pictures/Photos Library.photoslibrary/database/Photos.sqlite). "+
			"Must appear before the positional <storage>.")
	immichURL := fs.String("immich-url", "",
		"Immich server URL (only used by 'scan immich'). "+
			"Trailing /api is stripped automatically.")
	immichKeyEnv := fs.String("immich-api-key-env", "IMMICH_API_KEY",
		"name of the env var holding the Immich API key (only used by 'scan immich').")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = cfgPath

	if *all && fs.NArg() > 0 {
		fmt.Fprintf(stderr, "trove scan: --all conflicts with positional %q\n", fs.Arg(0))
		return 2
	}
	if *all {
		fmt.Fprintln(stderr, "trove scan --all: not implemented yet")
		return 1
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "trove scan: missing <storage> (or pass --all)")
		return 2
	}

	storage := fs.Arg(0)
	switch storage {
	case "photos":
		return scanPhotos(stdout, stderr, *library)
	case "immich":
		return scanImmich(stdout, stderr, *immichURL, *immichKeyEnv)
	default:
		fmt.Fprintf(stderr, "trove scan %s: not implemented yet\n", storage)
		return 1
	}
}

func scanPhotos(stdout, stderr io.Writer, libraryFlag string) int {
	path := libraryFlag
	if path == "" {
		p, err := defaultPhotosLibraryPath()
		if err != nil {
			fmt.Fprintf(stderr, "trove scan photos: resolve default library path: %v\n", err)
			return 1
		}
		path = p
	}

	lib, err := photosmacos.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "trove scan photos: %v\n", err)
		return 1
	}
	defer lib.Close()

	assets, err := lib.Assets(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "trove scan photos: %v\n", err)
		return 1
	}

	playback := map[photosmacos.PlaybackStyle]int{}
	resources := map[photosmacos.ResourceType]int{}
	noOriginal := 0
	for _, a := range assets {
		playback[a.PlaybackStyle]++
		if len(a.Resources) == 0 {
			noOriginal++
		}
		for _, r := range a.Resources {
			resources[r.Type]++
		}
	}
	fmt.Fprintln(stdout, "trove scan photos")
	fmt.Fprintf(stdout, "  library:       %s\n", lib.Path())
	fmt.Fprintf(stdout, "  join table:    %s\n", lib.JoinTable())
	fmt.Fprintf(stdout, "  active assets: %d\n", len(assets))
	fmt.Fprintf(stdout, "    stills:       %d\n", playback[photosmacos.PlaybackStill])
	fmt.Fprintf(stdout, "    animated:     %d\n", playback[photosmacos.PlaybackAnimated])
	fmt.Fprintf(stdout, "    live photos:  %d\n", playback[photosmacos.PlaybackLivePhoto])
	fmt.Fprintf(stdout, "    videos:       %d\n", playback[photosmacos.PlaybackVideo])
	fmt.Fprintf(stdout, "    slow-motion:  %d\n", playback[photosmacos.PlaybackSlowMotion])
	fmt.Fprintln(stdout, "  canonical resources surfaced:")
	fmt.Fprintf(stdout, "    photo originals:        %d\n", resources[photosmacos.ResourcePhoto])
	fmt.Fprintf(stdout, "    video originals:        %d\n", resources[photosmacos.ResourceVideo])
	fmt.Fprintf(stdout, "    live-motion originals:  %d\n", resources[photosmacos.ResourceLiveMotion])
	fmt.Fprintf(stdout, "    raw alternates:         %d\n", resources[photosmacos.ResourceAlternatePhoto])
	fmt.Fprintf(stdout, "  assets without canonical originals (iCloud-optimised or download-pending): %d\n", noOriginal)
	return 0
}

func scanImmich(stdout, stderr io.Writer, urlFlag, keyEnvName string) int {
	if urlFlag == "" {
		fmt.Fprintln(stderr, "trove scan immich: --immich-url is required")
		return 2
	}
	apiKey := os.Getenv(keyEnvName)
	if apiKey == "" {
		fmt.Fprintf(stderr, "trove scan immich: env %s is empty — set your Immich API key there (or override the var name with --immich-api-key-env)\n", keyEnvName)
		return 2
	}

	c, err := immichapi.Open(urlFlag, apiKey)
	if err != nil {
		fmt.Fprintf(stderr, "trove scan immich: %v\n", err)
		return 1
	}
	defer c.Close()

	assets, err := c.Assets(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "trove scan immich: %v\n", err)
		return 1
	}

	visibility := map[immichapi.Visibility]int{}
	types := map[immichapi.AssetType]int{}
	var trashed, bridged, livePaired int
	for _, a := range assets {
		visibility[a.Visibility]++
		types[a.Type]++
		if a.IsTrashed {
			trashed++
		}
		if a.DeviceAssetID != "" {
			bridged++
		}
		if a.LivePhotoVideoID != "" {
			livePaired++
		}
	}
	fmt.Fprintln(stdout, "trove scan immich")
	fmt.Fprintf(stdout, "  server:        %s\n", c.URL())
	fmt.Fprintf(stdout, "  total assets:  %d\n", len(assets))
	fmt.Fprintln(stdout, "  by type:")
	fmt.Fprintf(stdout, "    images:      %d\n", types[immichapi.TypeImage])
	fmt.Fprintf(stdout, "    videos:      %d\n", types[immichapi.TypeVideo])
	fmt.Fprintf(stdout, "    audio:       %d\n", types[immichapi.TypeAudio])
	fmt.Fprintf(stdout, "    other:       %d\n", types[immichapi.TypeOther])
	fmt.Fprintln(stdout, "  by visibility:")
	fmt.Fprintf(stdout, "    timeline:    %d\n", visibility[immichapi.VisibilityTimeline])
	fmt.Fprintf(stdout, "    archive:     %d\n", visibility[immichapi.VisibilityArchive])
	fmt.Fprintf(stdout, "    hidden:      %d\n", visibility[immichapi.VisibilityHidden])
	fmt.Fprintf(stdout, "    locked:      %d\n", visibility[immichapi.VisibilityLocked])
	fmt.Fprintf(stdout, "  trashed (isTrashed=true):    %d\n", trashed)
	fmt.Fprintf(stdout, "  with PHAsset bridge id:      %d  (of %d total; the rest were uploaded by paths other than the iOS app)\n", bridged, len(assets))
	fmt.Fprintf(stdout, "  Live Photo pairs:            %d  (stills with livePhotoVideoId pointing at their motion asset)\n", livePaired)
	return 0
}

func defaultPhotosLibraryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Pictures", "Photos Library.photoslibrary", "database", "Photos.sqlite"), nil
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	_ = stdout
	fs := newFlagSet("verify", stderr)
	force := fs.Bool("force", false, "ignore verification cache and re-check from scratch")
	cfgPath := fs.String("config", "", "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = force
	_ = cfgPath

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "trove verify: missing <flow>")
		return 2
	}
	fmt.Fprintf(stderr, "trove verify %s: not implemented yet\n", fs.Arg(0))
	return 1
}

func runCleanupReport(args []string, stdout, stderr io.Writer) int {
	_ = stdout
	fs := newFlagSet("cleanup-report", stderr)
	cfgPath := fs.String("config", "", "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = cfgPath

	fmt.Fprintln(stderr, "trove cleanup-report: not implemented yet")
	return 1
}

func runDeepcheck(args []string, stdout, stderr io.Writer) int {
	_ = stdout
	fs := newFlagSet("deepcheck", stderr)
	force := fs.Bool("force", false, "ignore verification cache and re-check from scratch")
	cfgPath := fs.String("config", "", "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = force
	_ = cfgPath

	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "trove deepcheck: missing <asset-id>")
		return 2
	}
	fmt.Fprintf(stderr, "trove deepcheck %s: not implemented yet\n", fs.Arg(0))
	return 1
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	_ = stdout
	fs := newFlagSet("status", stderr)
	cfgPath := fs.String("config", "", "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = cfgPath

	fmt.Fprintln(stderr, "trove status: not implemented yet")
	return 1
}
