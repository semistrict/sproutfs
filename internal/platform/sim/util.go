package sim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/internal/platform"
)

func sleep(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
			return nil
		}
	}
	return ctxsync.Sleep(ctx, duration)
}

func operationLatency(base time.Duration, bytes int, bytesPerSecond int64) time.Duration {
	if bytes <= 0 || bytesPerSecond <= 0 {
		return base
	}
	return base + time.Duration(int64(bytes)*int64(time.Second)/bytesPerSecond)
}

func validatePath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == "." || strings.HasPrefix(name, "../") {
		return platform.ErrInvalidPath
	}
	return nil
}

func etagOf(value []byte) platform.ETag {
	sum := sha256.Sum256(value)
	return platform.ETag(hex.EncodeToString(sum[:]))
}
