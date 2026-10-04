package real

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/sproutfs/platform"
)

// fakeCompute answers the few calls of the Compute Engine API the adapter
// makes, over disks and instances it keeps: one zone, operations that are
// running when they start and done when waited on.
type fakeCompute struct {
	mu sync.Mutex
	// users is, by disk, the instance it is attached to, and devices the
	// device name it was attached under there.
	users   map[string]string
	devices map[string]string
	sizes   map[string]int64
	calls   []string
}

func (f *fakeCompute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/compute/v1/projects/p/zones/z/")
	f.calls = append(f.calls, r.Method+" "+path)
	parts := strings.Split(path, "/")
	reply := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "disks":
		size, found := f.sizes[parts[1]]
		if !found {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		disk := map[string]any{"name": parts[1], "sizeGb": strconv.FormatInt(size, 10)}
		if user := f.users[parts[1]]; user != "" {
			disk["users"] = []string{"https://www.googleapis.com/compute/v1/projects/p/zones/z/instances/" + user}
		}
		reply(disk)
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "instances":
		var disks []map[string]any
		for disk, user := range f.users {
			if user == parts[1] {
				disks = append(disks, map[string]any{
					"source":     "https://www.googleapis.com/compute/v1/projects/p/zones/z/disks/" + disk,
					"deviceName": f.devices[disk]})
			}
		}
		reply(map[string]any{"name": parts[1], "disks": disks})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "attachDisk":
		var attached struct {
			Source, DeviceName, Mode string
			AutoDelete               bool
		}
		_ = json.NewDecoder(r.Body).Decode(&attached)
		disk := attached.Source[strings.LastIndex(attached.Source, "/")+1:]
		if attached.Mode != "READ_WRITE" || attached.AutoDelete || attached.DeviceName != disk {
			http.Error(w, `{"error":{"code":400,"message":"bad attach"}}`, http.StatusBadRequest)
			return
		}
		f.users[disk], f.devices[disk] = parts[1], attached.DeviceName
		reply(map[string]any{"name": "attach-" + disk, "status": "RUNNING"})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "detachDisk":
		device := r.URL.Query().Get("deviceName")
		for disk, name := range f.devices {
			if name == device && f.users[disk] == parts[1] {
				delete(f.users, disk)
				delete(f.devices, disk)
			}
		}
		reply(map[string]any{"name": "detach-" + device, "status": "RUNNING"})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "operations" && parts[2] == "wait":
		reply(map[string]any{"name": parts[1], "status": "DONE"})
	default:
		http.Error(w, `{"error":{"code":404,"message":"no such call"}}`, http.StatusNotFound)
	}
}

// The adapter describes, attaches and detaches a disk named by its CSI volume
// handle, under its own name as the device name, refuses a disk attached to
// another instance as in use, and does nothing where the disk already is.
func TestGCEDisksAttachAndDetachADiskByItsVolumeHandle(t *testing.T) {
	fake := &fakeCompute{users: map[string]string{}, devices: map[string]string{},
		sizes: map[string]int64{"shard-0": 100}}
	server := httptest.NewServer(fake)
	defer server.Close()
	ctx := t.Context()
	disks, err := NewGCEDisks(ctx, "", "", server.URL+"/compute/v1/")
	if err != nil {
		t.Fatal(err)
	}
	const volume = "projects/p/zones/z/disks/shard-0"
	described, err := disks.Describe(ctx, volume)
	if err != nil {
		t.Fatal(err)
	}
	if described.Bytes != 100<<30 || len(described.Machines) != 0 {
		t.Fatalf("a new disk is described as %+v, want 100 GiB attached nowhere", described)
	}
	if err := disks.Attach(ctx, volume, "node-a"); err != nil {
		t.Fatal(err)
	}
	if err := disks.Attach(ctx, volume, "node-a"); err != nil {
		t.Fatalf("attaching a disk where it is: %v", err)
	}
	if err := disks.Attach(ctx, volume, "node-b"); !errors.Is(err, platform.ErrInUse) {
		t.Fatalf("attaching a disk attached elsewhere: %v, want ErrInUse", err)
	}
	described, err = disks.Describe(ctx, volume)
	if err != nil || !slices.Equal(described.Machines, []string{"node-a"}) {
		t.Fatalf("an attached disk is described as %+v (%v), want on node-a", described, err)
	}
	if err := disks.Detach(ctx, volume, "node-b"); err != nil {
		t.Fatalf("detaching a disk from an instance it is not on: %v", err)
	}
	if err := disks.Detach(ctx, volume, "node-a"); err != nil {
		t.Fatal(err)
	}
	if fake.users["shard-0"] != "" {
		t.Fatalf("the disk is still attached to %q", fake.users["shard-0"])
	}
	if _, err := disks.Describe(ctx, "projects/p/zones/z/disks/gone"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("describing a disk that does not exist: %v, want ErrNotFound", err)
	}
	if _, err := disks.Describe(ctx, "shard-0"); !errors.Is(err, platform.ErrInvalidPath) {
		t.Fatalf("a bare name with no project and zone: %v, want ErrInvalidPath", err)
	}
	want := []string{"GET disks/shard-0", "GET disks/shard-0", "POST instances/node-a/attachDisk",
		"POST operations/attach-shard-0/wait"}
	if !slices.Equal(fake.calls[:len(want)], want) {
		t.Fatalf("the adapter called %v, want it to begin %v", fake.calls, want)
	}
}

func TestAGCEVolumeIsAHandleOrANameInTheConfiguredZone(t *testing.T) {
	for _, tc := range []struct {
		volume, project, zone string
		want                  gceVolume
		ok                    bool
	}{
		{"projects/p/zones/us-east4-a/disks/shard-1", "", "", gceVolume{"p", "us-east4-a", "shard-1"}, true},
		{"shard-1", "q", "us-central1-b", gceVolume{"q", "us-central1-b", "shard-1"}, true},
		{"shard-1", "", "", gceVolume{}, false},
		{"projects/p/regions/r/disks/shard-1", "", "", gceVolume{}, false},
		{"projects//zones/z/disks/shard-1", "", "", gceVolume{}, false},
	} {
		got, err := parseGCEVolume(tc.volume, tc.project, tc.zone)
		if tc.ok != (err == nil) || got != tc.want {
			t.Errorf("parseGCEVolume(%q, %q, %q) = %+v, %v; want %+v, ok %v", tc.volume, tc.project, tc.zone, got,
				err, tc.want, tc.ok)
		}
	}
	if got := GCEDeviceName("projects/p/zones/z/disks/shard-1"); got != "shard-1" {
		t.Fatalf("the device name of a handle is %q, want shard-1", got)
	}
}
