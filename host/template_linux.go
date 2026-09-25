//go:build linux && (amd64 || arm64)

package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/volume"
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

// templateOf returns the checkpoint every VM of one configured guest image is
// forked from, reading the image and asking the host for its template the first
// time it is asked for.
//
// The template is the deployment's rather than this process's: it is named by
// the image's own bytes, so a host that finds it already imported — by a
// predecessor of this pod, or by another host of the deployment — opens nothing
// and imports nothing. What is held in memory here is that answer, because it
// cannot change while this process runs: the file behind the name is read once,
// at startup, and a file that changed under a running host would be a template
// nothing has asked for.
func (s *supervisor) templateOf(ctx context.Context, name string, chosen Template) (*ImportedTemplate, error) {
	s.mu.Lock()
	cached := s.templates[name]
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	file, err := os.Open(chosen.Path)
	if err != nil {
		return nil, fmt.Errorf("guest image %s: %w", chosen.Path, err)
	}
	defer file.Close()
	prepared, err := s.importTemplate(ctx, file, chosen.MemoryBytes, name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.templates[name] = prepared
	s.mu.Unlock()
	return prepared, nil
}

// ImportTemplate imports a guest image a builder produced into the template its
// bytes name, on request. The template is the deployment's from then on: any
// host creates from it by its identity, and an image that is already imported
// costs one control record read. An image that is not a seekable file is
// staged under the scratch directory first, because an import reads it twice:
// once for the digest that names the template, and once for its bytes.
func (s *supervisor) ImportTemplate(ctx context.Context, image io.Reader,
	request hostapi.ImportTemplateRequest) (hostapi.ImportTemplateResult, error) {
	began := s.clock.Now()
	memory := request.Memory
	if memory == 0 {
		memory = s.config.VMMemoryBytes
	}
	if memory == 0 || memory%s.pagers.Ram.PageSize() != 0 {
		return hostapi.ImportTemplateResult{}, fmt.Errorf("%w: a template's RAM of %d bytes is not whole %d-byte pages",
			ErrRequest, memory, s.pagers.Ram.PageSize())
	}
	source, ok := image.(io.ReadSeeker)
	if !ok {
		staged, err := os.CreateTemp(s.config.ScratchDir, "template-*")
		if err != nil {
			return hostapi.ImportTemplateResult{}, fmt.Errorf("staging a guest image: %w", err)
		}
		defer func() {
			if err := errors.Join(staged.Close(), os.Remove(staged.Name())); err != nil {
				slog.WarnContext(ctx, "host: removing a staged guest image failed", "path", staged.Name(), "error", err)
			}
		}()
		if _, err := io.Copy(staged, image); err != nil {
			return hostapi.ImportTemplateResult{}, fmt.Errorf("staging a guest image: %w", err)
		}
		source = staged
	}
	imported, err := s.importTemplate(ctx, source, memory, "requested")
	if err != nil {
		return hostapi.ImportTemplateResult{}, err
	}
	s.mu.Lock()
	s.byID[imported.ID()] = imported
	s.mu.Unlock()
	return hostapi.ImportTemplateResult{Template: templateEntry(imported, imported.ID()),
		Checkpoint: imported.Point.Parent().Sequence, Seconds: s.since(began)}, nil
}

// importTemplate imports one guest image into the template its bytes name, at
// the RAM every VM created from it starts with. label is what the logs call the
// image. One import runs at a time: two creations of the first VM would
// otherwise both import the same image.
func (s *supervisor) importTemplate(ctx context.Context, source io.ReadSeeker, memory uint64,
	label string) (*ImportedTemplate, error) {
	length, err := source.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("guest image %s: %w", label, err)
	}
	// A PMEM device is whole pages of the pager that maps it, and the image is
	// written into the front of one.
	ramPage, pmemPage := s.pagers.Ram.PageSize(), s.pagers.Pmem.PageSize()
	size := (uint64(length) + pmemPage - 1) / pmemPage * pmemPage
	if size == 0 {
		return nil, fmt.Errorf("%w: guest image %s is empty", ErrRequest, label)
	}
	if err := s.templateMu.Lock(ctx); err != nil {
		return nil, err
	}
	defer s.templateMu.Unlock()
	prepared, err := s.host.TemplateOf(ctx, TemplateImport{
		Image: label,
		// Each volume is published in the page of the pager that will map it: a
		// template is forked into VMs this host runs, and a memory region a pager
		// could not serve is a VM it could not start.
		Volumes: []volume.VolumeSpec{
			{Name: vmmachine.RAMVolume, Size: memory, PageSize: ramPage},
			{Name: rootVolume, Size: size, PageSize: pmemPage},
		},
		Root: rootVolume, Source: source,
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.byID[prepared.ID()] = prepared
	s.mu.Unlock()
	slog.InfoContext(ctx, "host: a template is ready", "template", label, "vm", prepared.ID(),
		"checkpoint", prepared.Point.Parent().Sequence, "bytes", size)
	return prepared, nil
}

// templateNamed is the template a create selects, and the name it is reported
// by: a configured image by its name, or any template of the deployment by its
// identity, which is how a VM is created from an image imported on request on
// whichever host. The time is what reaching it cost, which is an import only
// for the first VM of a configured image.
func (s *supervisor) templateNamed(ctx context.Context, selected string) (*ImportedTemplate, string, error) {
	if hostapi.IsTemplate(selected) {
		s.mu.Lock()
		cached := s.byID[selected]
		s.mu.Unlock()
		if cached != nil {
			return cached, selected, nil
		}
		opened, err := s.host.Template(ctx, selected)
		if err != nil {
			if errors.Is(err, ErrUnknownTemplate) || errors.Is(err, ErrTemplatePending) {
				err = fmt.Errorf("%w: %w", ErrRequest, err)
			}
			return nil, "", err
		}
		s.mu.Lock()
		s.byID[selected] = opened
		s.mu.Unlock()
		return opened, selected, nil
	}
	name, chosen, err := s.config.Templates.Resolve(selected)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrRequest, err)
	}
	template, err := s.templateOf(ctx, name, chosen)
	return template, name, err
}

// ---------------------------------------------------------------------------
// Console
// ---------------------------------------------------------------------------
