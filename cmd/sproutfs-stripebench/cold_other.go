//go:build !linux

package main

import "os"

// dropCacheFunc is nil off Linux, where one range of a file cannot be dropped
// from the page cache. A server there serves from memory only.
func dropCacheFunc(*os.File) func(off, n int64) error { return nil }
