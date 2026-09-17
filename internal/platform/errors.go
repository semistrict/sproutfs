package platform

import "errors"

var (
	ErrAlreadyExists    = errors.New("already exists")
	ErrClosed           = errors.New("closed")
	ErrDisconnected     = errors.New("disconnected")
	ErrDiskFailed       = errors.New("disk failed")
	ErrInjectedFault    = errors.New("injected fault")
	ErrInvalidObjectKey = errors.New("invalid object key")
	ErrInvalidPath      = errors.New("invalid path")
	ErrInvalidRange     = errors.New("invalid range")
	ErrMessageTooLarge  = errors.New("message too large")
	ErrNotFound         = errors.New("not found")
	ErrNoSpace          = errors.New("filesystem space exhausted")
	// ErrFileUnchanged qualifies a namespace failure: target file identities
	// and contents did not change. Ancestor directories may have been created.
	ErrFileUnchanged  = errors.New("target files unchanged")
	ErrPrecondition   = errors.New("precondition failed")
	ErrProcessStopped = errors.New("process stopped")
	ErrStaleHandle    = errors.New("stale file handle")
	ErrUnavailable    = errors.New("unavailable")
)
