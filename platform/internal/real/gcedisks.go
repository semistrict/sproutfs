package real

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

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
// never deleted with the instance.
type GCEDisks struct {
	service        *compute.Service
	project, zone  string
	operationLimit time.Duration
}

// NewGCEDisks reaches Compute Engine with the ambient Google credentials, for
// volumes named by their name alone in project and zone. A non-empty endpoint
// points the client at a server that answers for the API, and sends no
// credentials: a test's.
func NewGCEDisks(ctx context.Context, project, zone, endpoint string) (*GCEDisks, error) {
	options := []option.ClientOption{}
	if endpoint != "" {
		options = append(options, option.WithEndpoint(endpoint), option.WithoutAuthentication(),
			option.WithHTTPClient(http.DefaultClient))
	}
	service, err := compute.NewService(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("compute engine client: %w", err)
	}
	return &GCEDisks{service: service, project: project, zone: zone, operationLimit: 10 * time.Minute}, nil
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

func (d *GCEDisks) volume(volume string) (gceVolume, error) { return parseGCEVolume(volume, d.project, d.zone) }

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
		if first.Code == "RESOURCE_IN_USE_BY_ANOTHER_RESOURCE" {
			return errors.Join(platform.ErrInUse, err)
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
		case api.Code == http.StatusTooManyRequests || api.Code >= 500:
			return errors.Join(platform.ErrUnavailable, fmt.Errorf("%s: %w", what, err))
		case api.Code == http.StatusBadRequest && strings.Contains(api.Message, "already being used"):
			return errors.Join(platform.ErrInUse, fmt.Errorf("%s: %w", what, err))
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

var _ platform.NetworkDisks = (*GCEDisks)(nil)
