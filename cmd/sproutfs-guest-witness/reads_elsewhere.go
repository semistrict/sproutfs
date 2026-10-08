//go:build !linux

package main

// oDirect is nothing where the system has no O_DIRECT: the reads then go
// through the page cache.
const oDirect = 0
