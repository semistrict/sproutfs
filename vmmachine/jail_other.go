//go:build !(linux && (amd64 || arm64))

package vmmachine

// Close has nothing to undo where no VMM can run.
func (j *Jail) Close() error { return nil }
