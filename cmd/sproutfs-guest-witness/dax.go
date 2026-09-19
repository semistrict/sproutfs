package main

import (
	"fmt"
	"os"
	"strings"
)

// A guest's disk is a PMEM device mounted with DAX, so that the guest maps the
// host's resident pages directly and keeps no page cache of its own: the host's
// one copy of a page is the only copy, however many guests inherited it. A
// filesystem on that device mounted any other way still works, and gives every
// guest a second copy of what the host already shares, which nothing a host can
// see would report. The witness is the one thing in a production guest placed
// to notice, so it refuses a file on PMEM that the kernel does not report as
// DAX, on every fill and every check.

// statxAttrDAX is STATX_ATTR_DAX: the file is in the DAX state, so its mappings
// and its reads bypass the page cache.
const statxAttrDAX = 0x00200000

// daxRequired refuses a witness file that lives on a PMEM device and is not
// DAX. device is the kernel's name for the block device under the file — pmem0,
// vda — and attributes the file's statx attributes. A file on anything that is
// not PMEM is asked for nothing: a witness run on a developer's machine, or
// over a tmpfs, has no PMEM under it.
func daxRequired(path, device string, attributes uint64) error {
	if !strings.HasPrefix(device, "pmem") || attributes&statxAttrDAX != 0 {
		return nil
	}
	return fmt.Errorf("the witness file %s is on %s and is not DAX: "+
		"this guest keeps a page cache of pages the host already shares", path, device)
}

// requireDAX is daxRequired for an open file.
func requireDAX(file *os.File, path string) error {
	device, attributes, err := deviceAndAttributes(file)
	if err != nil {
		return fmt.Errorf("the witness file %s: %w", path, err)
	}
	return daxRequired(path, device, attributes)
}
