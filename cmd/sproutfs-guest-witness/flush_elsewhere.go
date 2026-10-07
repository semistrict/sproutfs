//go:build !linux

package main

import "os"

// fdatasync is fsync where the system has no fdatasync.
func fdatasync(file *os.File) error { return file.Sync() }
