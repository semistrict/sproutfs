package wire

// ComputeChecksum is the streaming digest helper, reachable from the package's
// own tests alone. It has no caller in the codec or in vmmigrate — a page
// source checksums the buffer it already holds — so it is not part of the
// package's surface, and the checksum contract the tests state is stated over
// it rather than over a second implementation.
var ComputeChecksum = computeChecksum
