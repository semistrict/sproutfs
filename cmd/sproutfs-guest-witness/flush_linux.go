package main

import (
	"os"
	"syscall"
)

// fdatasync flushes a file's data and what reading it back needs, as a
// database flushing its log does.
func fdatasync(file *os.File) error { return syscall.Fdatasync(int(file.Fd())) }
