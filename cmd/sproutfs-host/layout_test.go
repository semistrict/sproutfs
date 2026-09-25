package main

import (
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// adapterChoosers is every package that may name platform/adapters
// outside a test file. Choosing an implementation of a platform port is a
// startup decision, so the commands that start something make it; internal
// packages take the port in their configuration and name no adapter, which is
// what keeps one from coming to depend on an adapter's own surface.
// internal/testnet is the exception the layout plan states: it is a package
// only tests import, and it builds the real loopback transport for them.
var adapterChoosers = []string{
	"github.com/semistrict/sproutfs/cmd/sproutfs-host",
	"github.com/semistrict/sproutfs/cmd/sproutfs-orchestrator",
	"github.com/semistrict/sproutfs/internal/testnet",
}

// TestOnlyTheCommandsChooseAnAdapter is the compiler check the layout plan
// cannot ask for: Go has no rule that says "importable, but only from these
// packages, and only outside their tests". Both operating systems are listed
// because half of this module's files are behind a Linux build tag.
func TestOnlyTheCommandsChooseAnAdapter(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			var named []string
			for _, pkg := range listPackages(t, goos) {
				if slices.Contains(pkg.Imports, adaptersPath) {
					named = append(named, pkg.ImportPath)
				}
			}
			slices.Sort(named)
			if !slices.Equal(named, adapterChoosers) {
				t.Fatalf("GOOS=%s: %s is imported outside a test by %v, want %v",
					goos, adaptersPath, named, adapterChoosers)
			}
		})
	}
}

const adaptersPath = "github.com/semistrict/sproutfs/platform/adapters"

// listedPackage is the part of `go list`'s package record this test reads.
// Imports is the package's own imports; a test file's are TestImports and
// XTestImports, which this deliberately leaves out.
type listedPackage struct {
	ImportPath string
	Imports    []string
}

func listPackages(t *testing.T, goos string) []listedPackage {
	t.Helper()
	// The module root, which is where ./... means the whole module.
	list := exec.Command("go", "list", "-json", "./...")
	list.Dir = "../.."
	list.Env = append(list.Environ(), "GOOS="+goos)
	out, err := list.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("go list for GOOS=%s: %v\n%s", goos, err, exit.Stderr)
		}
		t.Fatalf("go list for GOOS=%s: %v", goos, err)
	}
	var packages []listedPackage
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		packages = append(packages, pkg)
	}
	if len(packages) == 0 {
		t.Fatalf("go list for GOOS=%s named no packages", goos)
	}
	return packages
}
