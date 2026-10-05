package zirconvm

// This file is the one Go file of the package that is not ported, so it is
// the one file the rule below does not apply to.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// revision is the fuchsia commit the port is taken from, as a ported file's
// header names it.
const revision = "90e54e09"

// sourceList is every file under zircon/kernel at revision, with the copyright
// lines of its header. scripts/zircon-sources.py writes it from a clone, so the
// rule can be checked where there is none.
const sourceList = "testdata/zircon-kernel-files.txt"

// rule is this file, which the rule does not hold to itself.
const rule = "header_test.go"

// portedFrom is the line after the copyright lines, joined across the comment
// lines it wraps over. Its first path is under zircon/kernel/ and the rest are
// relative to zircon/kernel.
var portedFrom = regexp.MustCompile(
	`^Ported from zircon/kernel/(.+) at fuchsia (\S+)\. MIT licence; see LICENSE in this directory\.$`)

// Every ported file begins with the copyright lines of the Zircon files it is
// ported from, and a line naming those files at the revision. MIT code may be
// in this Apache-2.0 repository only with its notice beside it; the LICENSE in
// this directory is the notice, and the header says which code it covers.
func TestEveryFileNamesTheZirconSourcesItIsPortedFrom(t *testing.T) {
	sources := readSourceList(t)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || name == rule {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range headerProblems(source, sources) {
			t.Errorf("%s: %s", name, problem)
		}
	}
}

// The LICENSE beside the port is Zircon's: its copyright lines are the ones the
// source list records for zircon/kernel/LICENSE.
func TestTheLicenceIsZirconsKernelLicence(t *testing.T) {
	sources := readSourceList(t)
	licence, err := os.ReadFile("LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(licence), "\n")
	want := []string{"Copyright 2016 The Fuchsia Authors", "Copyright (c) 2008-2015 Travis Geiselbrecht"}
	if got := sources["LICENSE"]; !slices.Equal(got, want) {
		t.Errorf("the source list records LICENSE's copyright lines as %q, want %q", got, want)
	}
	if !slices.Equal(lines[:2], want) {
		t.Errorf("LICENSE begins %q, want %q", lines[:2], want)
	}
}

// The rule is only worth having if it fails on what it forbids, so it is run
// here over headers that break it one way each.
func TestTheHeaderRuleRejectsEachBrokenHeader(t *testing.T) {
	sources := readSourceList(t)
	cases := map[string]struct {
		source string
		want   []string
	}{
		"a ported file": {
			source: "// Copyright 2016 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/vm_page_list.cc and vm/include/vm/vm_page_list.h\n" +
				"// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.\n\npackage zirconvm\n",
		},
		"a file drawn from sources with different copyright lines": {
			source: "// Copyright 2016 The Fuchsia Authors\n" +
				"// Copyright (c) 2014 Travis Geiselbrecht\n" +
				"// Copyright 2020 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/include/vm/page.h, vm/page_queues.cc and\n" +
				"// vm/vm_page_list.cc at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.\n\npackage zirconvm\n",
		},
		"no header": {
			source: "package zirconvm\n",
			want:   []string{"has no copyright line", "has no Ported from line"},
		},
		"no Ported from line": {
			source: "// Copyright 2016 The Fuchsia Authors\n\npackage zirconvm\n",
			want:   []string{"has no Ported from line"},
		},
		"no copyright line": {
			source: "// Ported from zircon/kernel/vm/vm_page_list.cc\n" +
				"// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.\n\npackage zirconvm\n",
			want: []string{
				"has no copyright line",
				`carries copyright lines [], want those of its sources ["Copyright 2016 The Fuchsia Authors"]`,
			},
		},
		"another source's copyright line": {
			source: "// Copyright 2020 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/vm_page_list.cc\n" +
				"// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.\n\npackage zirconvm\n",
			want: []string{`carries copyright lines ["Copyright 2020 The Fuchsia Authors"], ` +
				`want those of its sources ["Copyright 2016 The Fuchsia Authors"]`},
		},
		"one of two copyright lines": {
			source: "// Copyright 2016 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/include/vm/page.h\n" +
				"// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.\n\npackage zirconvm\n",
			want: []string{`carries copyright lines ["Copyright 2016 The Fuchsia Authors"], ` +
				`want those of its sources ["Copyright 2016 The Fuchsia Authors" "Copyright (c) 2014 Travis Geiselbrecht"]`},
		},
		"another revision": {
			source: "// Copyright 2016 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/vm_page_list.cc\n" +
				"// at fuchsia 0123abcd. MIT licence; see LICENSE in this directory.\n\npackage zirconvm\n",
			want: []string{"names fuchsia 0123abcd, not 90e54e09"},
		},
		"a file that is not there": {
			source: "// Copyright 2016 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/vm_page_list.cc and vm/vm_page_lists.cc\n" +
				"// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.\n\npackage zirconvm\n",
			want: []string{"names zircon/kernel/vm/vm_page_lists.cc, which is not there at fuchsia 90e54e09"},
		},
		"a file the clone the list was written from does not hold": {
			source: "// Copyright 2016 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/OWNERS\n" +
				"// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.\n\npackage zirconvm\n",
			want: []string{"the source list has no copyright line for zircon/kernel/vm/OWNERS; " +
				"run scripts/zircon-sources.py on a clone that holds it"},
		},
		"no licence pointer": {
			source: "// Copyright 2016 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/vm_page_list.cc at fuchsia 90e54e09.\n\npackage zirconvm\n",
			want: []string{"has no Ported from line"},
		},
		"a header after the package clause": {
			source: "package zirconvm\n\n// Copyright 2016 The Fuchsia Authors\n" +
				"// Ported from zircon/kernel/vm/vm_page_list.cc\n" +
				"// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.\n",
			want: []string{"has no copyright line", "has no Ported from line"},
		},
	}
	for name, test := range cases {
		got := headerProblems([]byte(test.source), sources)
		if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", test.want) {
			t.Errorf("%s: the rule found %q, want %q", name, got, test.want)
		}
	}
}

// headerProblems is what is wrong with the header of one Go file, in the order
// the rule checks it.
func headerProblems(source []byte, sources map[string][]string) []string {
	// The header is the run of line comments the file begins with.
	var copyrights, rest []string
	for line := range strings.Lines(string(source)) {
		text, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "//")
		if !ok {
			break
		}
		text = strings.TrimSpace(text)
		if strings.HasPrefix(text, "Copyright ") && len(rest) == 0 {
			copyrights = append(copyrights, text)
			continue
		}
		rest = append(rest, text)
	}
	var problems []string
	if len(copyrights) == 0 {
		problems = append(problems, "has no copyright line")
	}
	match := portedFrom.FindStringSubmatch(strings.Join(rest, " "))
	if match == nil {
		return append(problems, "has no Ported from line")
	}
	if match[2] != revision {
		problems = append(problems, fmt.Sprintf("names fuchsia %s, not %s", match[2], revision))
	}
	paths := strings.Split(strings.ReplaceAll(match[1], " and ", ", "), ", ")
	var want []string
	for _, path := range paths {
		lines, ok := sources[path]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"names zircon/kernel/%s, which is not there at fuchsia %s", path, revision))
			continue
		}
		if len(lines) == 0 {
			problems = append(problems, fmt.Sprintf("the source list has no copyright line for "+
				"zircon/kernel/%s; run scripts/zircon-sources.py on a clone that holds it", path))
		}
		for _, line := range lines {
			if !slices.Contains(want, line) {
				want = append(want, line)
			}
		}
	}
	if len(want) > 0 && !slices.Equal(copyrights, want) {
		problems = append(problems, fmt.Sprintf(
			"carries copyright lines %q, want those of its sources %q", copyrights, want))
	}
	return problems
}

// readSourceList reads the source list: a path under zircon/kernel on each
// line, then the copyright lines of its header, separated by tabs.
func readSourceList(t *testing.T) map[string][]string {
	t.Helper()
	file, err := os.Open(sourceList)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	sources := map[string][]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		sources[fields[0]] = fields[1:]
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return sources
}
