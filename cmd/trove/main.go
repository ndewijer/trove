// Command trove is a read-only librarian that audits whether each photo is
// durably present across the user's existing backup chain. See SPEC.md.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
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
	_ = stdout
	fs := newFlagSet("scan", stderr)
	all := fs.Bool("all", false, "refresh all configured storages")
	cfgPath := fs.String("config", "", "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = cfgPath

	if *all {
		fmt.Fprintln(stderr, "trove scan --all: not implemented yet")
		return 1
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "trove scan: missing <storage> (or pass --all)")
		return 2
	}
	fmt.Fprintf(stderr, "trove scan %s: not implemented yet\n", fs.Arg(0))
	return 1
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
