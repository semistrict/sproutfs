// Package vmtest qualifies the Rust memory client against a real Linux Go
// pager fixture. The fixture is deliberately independent of volume storage;
// it is not the production page cache or a model of kernel fault behavior.
//
// readonly_linux_test.go proves on the running kernel what the isolated arena
// needs of read-only files, with the pager and a VMM of its own and no Rust
// client.
package vmtest
