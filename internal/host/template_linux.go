//go:build linux && (amd64 || arm64)

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/volume"
)

// importing makes sure every configured guest image has a published template,
// retrying until it does or the host closes. A host is ready when every image
// it is configured with is in a template, whoever imported it: an image another
// host has already imported costs this one a read of one control record. A host
// that cannot read its images stays unready rather than taking VMs it would
// fail to create.
func (s *supervisor) importing(ctx context.Context) {
	for {
		var failures []error
		for _, name := range slices.Sorted(maps.Keys(s.config.Templates)) {
			if _, err := s.templateOf(ctx, name, s.config.Templates[name]); err != nil {
				failures = append(failures, fmt.Errorf("importing %s: %w", name, err))
			}
		}
		if len(failures) == 0 {
			s.importErr.Store(nil)
			slog.InfoContext(ctx, "host: every guest image is imported", "templates", len(s.config.Templates))
			return
		}
		failure := errors.Join(failures...)
		s.notReady(failure)
		slog.ErrorContext(ctx, "host: importing the guest images failed", "error", failure)
		if err := s.clock.Sleep(ctx, importRetry); err != nil {
			return
		}
	}
}

// importRetry is how long a host waits before trying an import that failed
// again. It is short because the host is unready until it succeeds.
const importRetry = 5 * time.Second

// templateOf returns the checkpoint every VM of one guest image is forked from,
// reading the image and asking the host for its template the first time it is
// asked for.
//
// The template is the deployment's rather than this process's: it is named by
// the image's own bytes, so a host that finds it already imported — by a
// predecessor of this pod, or by another host of the deployment — opens nothing
// and imports nothing. What is held in memory here is that answer, because it
// cannot change while this process runs: the file behind the name is read once,
// at startup, and a file that changed under a running host would be a template
// nothing has asked for.
func (s *supervisor) templateOf(ctx context.Context, name string, chosen Template) (*ImportedTemplate, error) {
	path := chosen.Path
	s.mu.Lock()
	cached := s.templates[name]
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	if err := s.templateMu.Lock(ctx); err != nil {
		return nil, err
	}
	defer s.templateMu.Unlock()
	s.mu.Lock()
	cached = s.templates[name]
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("guest image %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("guest image %s: %w", path, err)
	}
	// A PMEM device is whole pages of the pager that maps it, and the image is
	// written into the front of one.
	ramPage, pmemPage := s.pagers.Ram.PageSize(), s.pagers.Pmem.PageSize()
	size := (uint64(info.Size()) + pmemPage - 1) / pmemPage * pmemPage
	if size == 0 {
		return nil, fmt.Errorf("guest image %s is empty", path)
	}
	prepared, err := s.host.TemplateOf(ctx, TemplateImport{
		Image: name,
		// Each volume is published in the page of the pager that will map it: a
		// template is forked into VMs this host runs, and a memory region a pager could
		// not serve is a VM it could not start.
		Volumes: []volume.VolumeSpec{
			{Name: vmmachine.RAMVolume, Size: chosen.MemoryBytes, PageSize: ramPage},
			{Name: rootVolume, Size: size, PageSize: pmemPage},
		},
		Root: rootVolume, Source: file,
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.templates[name] = prepared
	s.mu.Unlock()
	slog.InfoContext(ctx, "host: a template is ready", "template", name, "vm", prepared.ID(),
		"checkpoint", prepared.Point.Parent().Sequence, "bytes", size)
	return prepared, nil
}

// ---------------------------------------------------------------------------
// Console
// ---------------------------------------------------------------------------
