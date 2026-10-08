package main

import "syscall"

// oDirect opens a file for reads that skip the page cache, which on a DAX
// file every read does anyway.
const oDirect = syscall.O_DIRECT
