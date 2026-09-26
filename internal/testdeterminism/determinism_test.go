// Package testdeterminism holds the no-cheating rule: a check that the code a
// simulation runs draws its randomness and its time from the seams the
// simulation controls, and nowhere else.
//
// FoundationDB splits its random source in two so that a nondeterministic draw
// cannot influence simulated state. sproutfs has one clock port and one entropy
// port; this is what keeps a new call site from quietly reaching past them. A
// stray time.Now decides how long a hold lives, a stray math/rand decides which
// VM checkpoints first, and a run that cannot reproduce either is a run whose
// seed reports nothing.
package testdeterminism

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// checked are the packages the rule applies to: everything that decides what
// becomes durable, who may write it, and when. Subdirectories are included.
var checked = []string{
	"volume",
	"checkpoint",
	"control",
	"vmmigrate",
	"host",
	"vmmemory",
	// When a handoff is given up decides whether a stopped guest's writes
	// since its last checkpoint survive.
	"internal/handover",
}

// forbiddenImports are the packages that draw from a source no seed reaches.
// The port that replaces them is platform.Entropy.
var forbiddenImports = map[string]string{
	"math/rand":    "platform.Entropy",
	"math/rand/v2": "platform.Entropy",
	"crypto/rand":  "platform.Entropy",
}

// forbiddenCalls are the standard library's own readings of the wall clock.
// Every one of them has a platform.Clock method of the same name.
var forbiddenCalls = map[string]bool{
	"time.Now": true, "time.Since": true, "time.After": true, "time.Sleep": true,
	"time.NewTimer": true, "time.NewTicker": true, "time.Tick": true, "time.AfterFunc": true,
	// ctxsync.Sleep is a channel wait on a standard-library timer, so it is the
	// same reading wearing the repository's own name.
	"ctxsync.Sleep": true,
}

// allowed is every call site that may keep its own reading, with the reason it
// may. The count is part of the entry: a second reading added to an
// already-listed file is a new decision and has to be argued for here.
//
// Nothing in this table influences what becomes durable. A justification that
// cannot say that does not belong here; the call site belongs on a clock.
var allowed = map[string]allowance{
	// A socket deadline is an instant the kernel compares against. It is not a
	// duration this process measures, and no clock this process is given can be
	// handed to the operating system, so both readings stay on the wall clock.
	// Neither decides anything about a VM: each bounds one exchange with the
	// pager's client, and a client that misses it fails that command.
	"vmmemory/connection_linux.go:time.Now": {2,
		"kernel socket deadlines, which the operating system compares against its own clock"},
	// The audit ring exists only under the sproutfsprobe build tag, and all it
	// dates is the post-mortem dump it prints after a guest has already died.
	// No build a deployment or a campaign runs contains it, and nothing it
	// records reaches a decision, let alone a durable one.
	"vmmemory/probe_on.go:time.Now": {1,
		"the audit ring's post-mortem timestamps, behind a build tag no deployment uses"},
}

type allowance struct {
	count  int
	reason string
}

// finding is one reading the rule objects to.
type finding struct {
	// key is the file and the thing read, which is what the allowlist is keyed
	// by; line is where, and complaint says what to do instead.
	key       string
	line      int
	complaint string
}

func TestProductionCodeDrawsTimeAndRandomnessFromTheInjectedPorts(t *testing.T) {
	root := repoRoot(t)
	seen := map[string]int{}
	for _, dir := range checked {
		walk(t, root, dir, func(relative string, file *ast.File, fset *token.FileSet) {
			for _, found := range scan(relative, file, fset) {
				seen[found.key]++
				entry, listed := allowed[found.key]
				if listed && seen[found.key] <= entry.count {
					continue
				}
				t.Errorf("%s:%d: %s\n\tif it cannot be, allow it in internal/testdeterminism with the reason",
					relative, found.line, found.complaint)
			}
		})
	}
	// An allowance nothing needs any more is a justification for a call site
	// that is gone, which is exactly the note that later gets copied onto a new
	// one without being reread.
	for key, entry := range allowed {
		if seen[key] < entry.count {
			t.Errorf("%s: allowed %d reading(s), found %d; drop what is no longer there\n\treason: %s",
				key, entry.count, seen[key], entry.reason)
		}
	}
}

// The rule is only worth having if it fails on the thing it forbids, so it is
// run here over sources that contain exactly those things.
func TestTheRuleRejectsEachKindOfStrayReading(t *testing.T) {
	cases := map[string]struct {
		source string
		want   []string
	}{
		"a wall-clock reading": {
			source: "package host\nimport \"time\"\nfunc f() { _ = time.Now() }\n",
			want:   []string{"host/x.go:time.Now"},
		},
		"a timer nothing can advance": {
			source: "package host\nimport \"time\"\nfunc f() { _ = time.NewTimer(0); time.Sleep(0) }\n",
			want:   []string{"host/x.go:time.NewTimer", "host/x.go:time.Sleep"},
		},
		"a sleep wearing the repository's own name": {
			source: "package host\nimport \"context\"\nimport \"github.com/semistrict/sproutfs/internal/ctxsync\"\nfunc f(ctx context.Context) { _ = ctxsync.Sleep(ctx, 0) }\n",
			want:   []string{"host/x.go:ctxsync.Sleep"},
		},
		"an unseeded random source": {
			source: "package host\nimport \"math/rand/v2\"\nfunc f() int { return rand.IntN(2) }\n",
			want:   []string{"host/x.go:import math/rand/v2"},
		},
		"the operating system's entropy": {
			source: "package control\nimport \"crypto/rand\"\nfunc f(b []byte) { _, _ = rand.Read(b) }\n",
			want:   []string{"control/x.go:import crypto/rand"},
		},
		"a clock, which is the whole point": {
			source: "package host\nimport \"github.com/semistrict/sproutfs/platform\"\nfunc f(c platform.Clock) { _ = c.Now() }\n",
			want:   nil,
		},
	}
	for name, test := range cases {
		fset := token.NewFileSet()
		relative := "host/x.go"
		if strings.HasPrefix(test.source, "package control") {
			relative = "control/x.go"
		}
		file, err := parser.ParseFile(fset, relative, test.source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var keys []string
		for _, found := range scan(relative, file, fset) {
			keys = append(keys, found.key)
		}
		if fmt.Sprint(keys) != fmt.Sprint(test.want) {
			t.Errorf("%s: the rule found %v, want %v", name, keys, test.want)
		}
	}
}

// scan reports every forbidden import and call in one parsed file, in source
// order.
func scan(relative string, file *ast.File, fset *token.FileSet) []finding {
	var findings []finding
	for _, spec := range file.Imports {
		name, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if replacement, forbidden := forbiddenImports[name]; forbidden {
			findings = append(findings, finding{
				key:       relative + ":import " + name,
				line:      fset.Position(spec.Pos()).Line,
				complaint: "imports " + name + "; draw from " + replacement + " instead",
			})
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := qualifiedName(call.Fun)
		if !ok || !forbiddenCalls[name] {
			return true
		}
		findings = append(findings, finding{
			key:       relative + ":" + name,
			line:      fset.Position(call.Pos()).Line,
			complaint: "calls " + name + "; take it from the injected platform.Clock instead",
		})
		return true
	})
	return findings
}

// qualifiedName renders pkg.Name for a selector on a bare identifier, which is
// what every call this rule names looks like.
func qualifiedName(fun ast.Expr) (string, bool) {
	selector, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return pkg.Name + "." + selector.Sel.Name, true
}

// walk visits every non-test, non-generated Go file under one directory. Build
// tags are ignored deliberately: a Linux-only file is production code on the
// machine a deployment runs, and a rule that only saw this machine's files
// would let the host's own lifecycle escape it.
func walk(t *testing.T, root, dir string, visit func(string, *ast.File, *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Generated protobuf code is not written here and draws nothing.
			if entry.Name() == "gen" {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(relative), file, fset)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// This package sits two levels below the module root.
	return filepath.Dir(filepath.Dir(working))
}
