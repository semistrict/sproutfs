package real

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/semistrict/sproutfs/platform"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// GCEDisks is Compute Engine's API for persistent disks and Hyperdisks, as
// platform.NetworkDisks. A volume is named as a CSI volume handle names it,
// projects/<project>/zones/<zone>/disks/<name>, or by its name alone in the
// adapter's own project and zone. A machine is an instance's name, in the
// volume's zone: the name a GKE node, or a k3s node on its own instance, has.
// A disk is attached under its own name as its device name, so the instance
// sees it at /dev/disk/by-id/google-<name> (GCEDevices), read and write, and
// never deleted with the instance. List looks in the adapter's own project and
// zone, and Create makes a disk of the adapter's type.
type GCEDisks struct {
	service        *compute.Service
	config         GCEDisksConfig
	operationLimit time.Duration
}

// GCEDisksConfig is where an adapter's disks are and what it creates.
type GCEDisksConfig struct {
	// Project and Zone are where a disk named by its name alone is, and
	// where List looks.
	Project, Zone string
	// Endpoint, where non-empty, points the client at a server that answers
	// for the API, and sends no credentials: a test's.
	Endpoint string
	// DiskType is the type of disk Create makes, and IOPS and ThroughputMBps
	// what it provisions for it; zero leaves the cloud's default. An empty
	// DiskType is Hyperdisk Balanced with 6000 IOPS and 400 MB/s.
	DiskType             string
	IOPS, ThroughputMBps int64
}

// NewGCEDisks reaches Compute Engine with the ambient Google credentials, or
// with none where config names an endpoint. Without an endpoint, a project or
// zone config leaves empty is the instance's own, from the metadata server.
func NewGCEDisks(ctx context.Context, config GCEDisksConfig) (*GCEDisks, error) {
	options := []option.ClientOption{}
	if config.Endpoint != "" {
		options = append(options, option.WithEndpoint(config.Endpoint), option.WithoutAuthentication(),
			option.WithHTTPClient(http.DefaultClient))
	} else {
		config = locateGCEDisks(ctx, config, metadata.NewClient(nil))
	}
	service, err := compute.NewService(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("compute engine client: %w", err)
	}
	if config.DiskType == "" {
		config.DiskType, config.IOPS, config.ThroughputMBps = "hyperdisk-balanced", 6000, 400
	}
	return &GCEDisks{service: service, config: config, operationLimit: 10 * time.Minute}, nil
}

// gceMetadataLimit bounds the metadata server's answers. Off Compute Engine
// there is no server, and the lookup gives up rather than hold up a start.
const gceMetadataLimit = 5 * time.Second

// locateGCEDisks fills in the project and zone config leaves empty with the
// instance's own, as the metadata server gives them. A controller names the
// disks it creates and lists by their names alone, which mean nothing without
// them. A server that does not answer leaves them empty, and a call that
// needs them then says so.
func locateGCEDisks(ctx context.Context, config GCEDisksConfig, server *metadata.Client) GCEDisksConfig {
	if config.Project != "" && config.Zone != "" {
		return config
	}
	ctx, cancel := context.WithTimeout(ctx, gceMetadataLimit)
	defer cancel()
	if config.Project == "" {
		project, err := server.GetWithContext(ctx, "project/project-id")
		if err != nil {
			slog.WarnContext(ctx, "compute engine: the metadata server did not say the project", "error", err)
		}
		config.Project = strings.TrimSpace(project)
	}
	if config.Zone == "" {
		// projects/<number>/zones/<zone>
		zone, err := server.GetWithContext(ctx, "instance/zone")
		if err != nil {
			slog.WarnContext(ctx, "compute engine: the metadata server did not say the zone", "error", err)
		}
		config.Zone = path.Base(strings.TrimSpace(zone))
		if config.Zone == "." {
			config.Zone = ""
		}
	}
	return config
}

// gceVolume is a disk as the API names it.
type gceVolume struct {
	project, zone, name string
}

// parseGCEVolume reads a volume's name: a CSI volume handle, or a disk's name
// alone in project and zone.
func parseGCEVolume(volume, project, zone string) (gceVolume, error) {
	parts := strings.Split(volume, "/")
	switch {
	case len(parts) == 1 && volume != "" && project != "" && zone != "":
		return gceVolume{project: project, zone: zone, name: volume}, nil
	case len(parts) == 6 && parts[0] == "projects" && parts[2] == "zones" && parts[4] == "disks" &&
		parts[1] != "" && parts[3] != "" && parts[5] != "":
		return gceVolume{project: parts[1], zone: parts[3], name: parts[5]}, nil
	}
	return gceVolume{}, fmt.Errorf("%w: %q is not projects/<project>/zones/<zone>/disks/<name>, or a disk's name "+
		"in a configured project and zone", platform.ErrInvalidPath, volume)
}

// GCEDeviceName is the device name a volume is attached under, and so the
// suffix of its path under /dev/disk/by-id/google-: the disk's own name.
func GCEDeviceName(volume string) string { return path.Base(volume) }

func (d *GCEDisks) volume(volume string) (gceVolume, error) {
	return parseGCEVolume(volume, d.config.Project, d.config.Zone)
}

// Describe reads a disk's size and the instances it is attached to.
func (d *GCEDisks) Describe(ctx context.Context, volume string) (platform.NetworkDisk, error) {
	v, err := d.volume(volume)
	if err != nil {
		return platform.NetworkDisk{}, err
	}
	disk, err := d.service.Disks.Get(v.project, v.zone, v.name).Context(ctx).Do()
	if err != nil {
		return platform.NetworkDisk{}, gceError(err, "describing "+volume)
	}
	described := platform.NetworkDisk{Bytes: disk.SizeGb << 30}
	for _, user := range disk.Users {
		described.Machines = append(described.Machines, path.Base(user))
	}
	return described, nil
}

// Attach attaches a disk to an instance, read and write, under its own name,
// and waits for the operation. A disk attached to another instance is
// platform.ErrInUse.
func (d *GCEDisks) Attach(ctx context.Context, volume, machine string) error {
	v, err := d.volume(volume)
	if err != nil {
		return err
	}
	described, err := d.Describe(ctx, volume)
	if err != nil {
		return err
	}
	for _, attached := range described.Machines {
		if attached == machine {
			return nil
		}
		return fmt.Errorf("%w: %s is attached to %s, not %s", platform.ErrInUse, volume, attached, machine)
	}
	source := fmt.Sprintf("projects/%s/zones/%s/disks/%s", v.project, v.zone, v.name)
	operation, err := d.service.Instances.AttachDisk(v.project, v.zone, machine, &compute.AttachedDisk{
		Source: source, DeviceName: v.name, Mode: "READ_WRITE", AutoDelete: false, Type: "PERSISTENT"}).
		Context(ctx).Do()
	if err != nil {
		return gceError(err, fmt.Sprintf("attaching %s to %s", volume, machine))
	}
	return d.wait(ctx, v, operation, fmt.Sprintf("attaching %s to %s", volume, machine))
}

// Detach detaches a disk from an instance, by the device name it was attached
// under there, and waits for the operation. A disk not attached to the
// instance is left alone.
func (d *GCEDisks) Detach(ctx context.Context, volume, machine string) error {
	v, err := d.volume(volume)
	if err != nil {
		return err
	}
	instance, err := d.service.Instances.Get(v.project, v.zone, machine).Context(ctx).Do()
	if errors.Is(gceError(err, ""), platform.ErrNotFound) {
		// An instance that is gone took no disk with it: the disk is attached
		// to nothing there.
		return nil
	}
	if err != nil {
		return gceError(err, "reading instance "+machine)
	}
	device := ""
	for _, attached := range instance.Disks {
		if path.Base(attached.Source) == v.name && strings.Contains(attached.Source, "/zones/"+v.zone+"/") {
			device = attached.DeviceName
		}
	}
	if device == "" {
		return nil
	}
	operation, err := d.service.Instances.DetachDisk(v.project, v.zone, machine, device).Context(ctx).Do()
	if err != nil {
		return gceError(err, fmt.Sprintf("detaching %s from %s", volume, machine))
	}
	return d.wait(ctx, v, operation, fmt.Sprintf("detaching %s from %s", volume, machine))
}

// List lists the disks of the adapter's own project and zone labelled
// key=value, page by page, each by its name alone.
func (d *GCEDisks) List(ctx context.Context, key, value string) ([]platform.ListedDisk, error) {
	project, zone := d.config.Project, d.config.Zone
	if project == "" || zone == "" {
		return nil, fmt.Errorf("%w: listing disks needs a configured project and zone", platform.ErrInvalidPath)
	}
	var listed []platform.ListedDisk
	filter := fmt.Sprintf("labels.%s = %q", key, value)
	err := d.service.Disks.List(project, zone).Filter(filter).Pages(ctx, func(page *compute.DiskList) error {
		for _, disk := range page.Items {
			found := platform.ListedDisk{Name: disk.Name, Bytes: disk.SizeGb << 30, Labels: disk.Labels}
			for _, user := range disk.Users {
				found.Machines = append(found.Machines, path.Base(user))
			}
			listed = append(listed, found)
		}
		return nil
	})
	if err != nil {
		return nil, gceError(err, fmt.Sprintf("listing disks labelled %s=%s", key, value))
	}
	slices.SortFunc(listed, func(a, b platform.ListedDisk) int { return strings.Compare(a.Name, b.Name) })
	return listed, nil
}

// Create inserts a disk of the adapter's type, its size rounded up to whole
// GiB, and waits for the operation. A disk of that name already there is
// platform.ErrAlreadyExists.
func (d *GCEDisks) Create(ctx context.Context, spec platform.NetworkDiskSpec) error {
	v, err := d.volume(spec.Name)
	if err != nil {
		return err
	}
	if spec.Bytes <= 0 {
		return fmt.Errorf("%w: a disk %q of %d bytes", platform.ErrInvalidPath, spec.Name, spec.Bytes)
	}
	disk := &compute.Disk{Name: v.name, SizeGb: (spec.Bytes + 1<<30 - 1) >> 30, Labels: spec.Labels,
		Type:            fmt.Sprintf("projects/%s/zones/%s/diskTypes/%s", v.project, v.zone, d.config.DiskType),
		ProvisionedIops: d.config.IOPS, ProvisionedThroughput: d.config.ThroughputMBps}
	operation, err := d.service.Disks.Insert(v.project, v.zone, disk).Context(ctx).Do()
	if err != nil {
		return gceError(err, "creating "+spec.Name)
	}
	return d.wait(ctx, v, operation, "creating "+spec.Name)
}

// Delete deletes a disk and waits for the operation. A disk attached to an
// instance is platform.ErrInUse.
func (d *GCEDisks) Delete(ctx context.Context, volume string) error {
	v, err := d.volume(volume)
	if err != nil {
		return err
	}
	operation, err := d.service.Disks.Delete(v.project, v.zone, v.name).Context(ctx).Do()
	if err != nil {
		return gceError(err, "deleting "+volume)
	}
	return d.wait(ctx, v, operation, "deleting "+volume)
}

// wait waits for a zonal operation to finish, and reports its error.
func (d *GCEDisks) wait(ctx context.Context, v gceVolume, operation *compute.Operation, what string) error {
	ctx, cancel := context.WithTimeout(ctx, d.operationLimit)
	defer cancel()
	for operation.Status != "DONE" {
		next, err := d.service.ZoneOperations.Wait(v.project, v.zone, operation.Name).Context(ctx).Do()
		if err != nil {
			return gceError(err, what)
		}
		operation = next
	}
	if operation.Error != nil && len(operation.Error.Errors) > 0 {
		first := operation.Error.Errors[0]
		err := fmt.Errorf("%s: %s: %s", what, first.Code, first.Message)
		switch first.Code {
		case "RESOURCE_IN_USE_BY_ANOTHER_RESOURCE":
			return errors.Join(platform.ErrInUse, err)
		case "RESOURCE_ALREADY_EXISTS":
			return errors.Join(platform.ErrAlreadyExists, err)
		}
		return err
	}
	return nil
}

// gceError maps an API error onto the platform's.
func gceError(err error, what string) error {
	if err == nil {
		return nil
	}
	var api *googleapi.Error
	if errors.As(err, &api) {
		switch {
		case api.Code == http.StatusNotFound:
			return errors.Join(platform.ErrNotFound, fmt.Errorf("%s: %w", what, err))
		case api.Code == http.StatusConflict:
			return errors.Join(platform.ErrAlreadyExists, fmt.Errorf("%s: %w", what, err))
		case api.Code == http.StatusTooManyRequests || api.Code >= 500:
			return errors.Join(platform.ErrUnavailable, fmt.Errorf("%s: %w", what, err))
		case api.Code == http.StatusBadRequest &&
			(hasReason(api, "resourceInUseByAnotherResource") || strings.Contains(api.Message, "already being used")):
			return errors.Join(platform.ErrInUse, fmt.Errorf("%s: %w", what, err))
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// hasReason reports whether one of an API error's errors gives reason.
func hasReason(api *googleapi.Error, reason string) bool {
	return slices.ContainsFunc(api.Errors, func(item googleapi.ErrorItem) bool { return item.Reason == reason })
}

var _ platform.NetworkDisks = (*GCEDisks)(nil)
