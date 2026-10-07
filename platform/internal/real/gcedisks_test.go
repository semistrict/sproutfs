package real

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	compute "google.golang.org/api/compute/v1"
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
	labels  map[string]map[string]string
	calls   []string
	// inserted is every disk an insert asked for, as it was sent.
	inserted []compute.Disk
}

// listed answers a disks.list: the disks whose labels match a filter of the
// form labels.<key> = "<value>", one to a page and in reverse name order, so
// the adapter has to follow the pages and sort them.
func (f *fakeCompute) listed(r *http.Request) (map[string]any, bool) {
	var key, value string
	if _, err := fmt.Sscanf(r.URL.Query().Get("filter"), "labels.%s = %q", &key, &value); err != nil {
		return nil, false
	}
	var names []string
	for name, labels := range f.labels {
		if labels[key] == value {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	slices.Reverse(names)
	page := 0
	if token := r.URL.Query().Get("pageToken"); token != "" {
		var err error
		if page, err = strconv.Atoi(token); err != nil {
			return nil, false
		}
	}
	reply := map[string]any{"items": []map[string]any{}}
	if page < len(names) {
		name := names[page]
		disk := map[string]any{"name": name, "sizeGb": strconv.FormatInt(f.sizes[name], 10), "labels": f.labels[name]}
		if user := f.users[name]; user != "" {
			disk["users"] = []string{"https://www.googleapis.com/compute/v1/projects/p/zones/z/instances/" + user}
		}
		reply["items"] = []map[string]any{disk}
	}
	if page+1 < len(names) {
		reply["nextPageToken"] = strconv.Itoa(page + 1)
	}
	return reply, true
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
	case r.Method == http.MethodGet && len(parts) == 1 && parts[0] == "disks":
		listed, ok := f.listed(r)
		if !ok {
			http.Error(w, `{"error":{"code":400,"message":"bad filter"}}`, http.StatusBadRequest)
			return
		}
		reply(listed)
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "disks":
		var disk compute.Disk
		if err := json.NewDecoder(r.Body).Decode(&disk); err != nil {
			http.Error(w, `{"error":{"code":400,"message":"bad disk"}}`, http.StatusBadRequest)
			return
		}
		f.inserted = append(f.inserted, disk)
		if _, found := f.sizes[disk.Name]; found {
			http.Error(w, `{"error":{"code":409,"message":"The resource already exists",`+
				`"errors":[{"reason":"alreadyExists"}]}}`, http.StatusConflict)
			return
		}
		f.sizes[disk.Name], f.labels[disk.Name] = disk.SizeGb, disk.Labels
		reply(map[string]any{"name": "insert-" + disk.Name, "status": "RUNNING"})
	case r.Method == http.MethodDelete && len(parts) == 2 && parts[0] == "disks":
		if _, found := f.sizes[parts[1]]; !found {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		if user := f.users[parts[1]]; user != "" {
			http.Error(w, fmt.Sprintf(`{"error":{"code":400,"message":"The disk resource `+
				`'projects/p/zones/z/disks/%s' is already being used by 'projects/p/zones/z/instances/%s'",`+
				`"errors":[{"reason":"resourceInUseByAnotherResource"}]}}`, parts[1], user), http.StatusBadRequest)
			return
		}
		delete(f.sizes, parts[1])
		delete(f.labels, parts[1])
		reply(map[string]any{"name": "delete-" + parts[1], "status": "RUNNING"})
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
	disks, err := NewGCEDisks(ctx, GCEDisksConfig{Endpoint: server.URL + "/compute/v1/"})
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

// The adapter inserts a disk of its type with its labels and waits for the
// operation, lists disks by label across pages in name order, and deletes a
// disk, mapping an attached disk to ErrInUse, a missing one to ErrNotFound
// and a second insert of a name to ErrAlreadyExists.
func TestGCEDisksCreateListAndDeleteDisksByLabel(t *testing.T) {
	fake := &fakeCompute{users: map[string]string{}, devices: map[string]string{},
		sizes: map[string]int64{"shard-0": 100}, labels: map[string]map[string]string{}}
	server := httptest.NewServer(fake)
	defer server.Close()
	ctx := t.Context()
	disks, err := NewGCEDisks(ctx, GCEDisksConfig{Project: "p", Zone: "z", Endpoint: server.URL + "/compute/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	east := map[string]string{"sproutfs-journal": "east"}
	for _, spec := range []platform.NetworkDiskSpec{
		{Name: "sproutfs-journal-b", Bytes: 32 << 30, Labels: east},
		{Name: "projects/p/zones/z/disks/sproutfs-journal-a", Bytes: 32<<30 + 1, Labels: east},
		{Name: "sproutfs-journal-c", Bytes: 32 << 30, Labels: map[string]string{"sproutfs-journal": "west"}},
	} {
		if err := disks.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	want := compute.Disk{Name: "sproutfs-journal-a", SizeGb: 33, Labels: east,
		Type:            "projects/p/zones/z/diskTypes/hyperdisk-balanced",
		ProvisionedIops: 6000, ProvisionedThroughput: 400}
	if !reflect.DeepEqual(fake.inserted[1], want) {
		t.Fatalf("the adapter inserted %+v, want %+v", fake.inserted[1], want)
	}
	err = disks.Create(ctx, platform.NetworkDiskSpec{Name: "sproutfs-journal-a", Bytes: 32 << 30})
	if !errors.Is(err, platform.ErrAlreadyExists) {
		t.Fatalf("inserting a disk of a name the zone has: %v, want ErrAlreadyExists", err)
	}
	fake.users["sproutfs-journal-b"] = "node-a"
	listed, err := disks.List(ctx, "sproutfs-journal", "east")
	if err != nil {
		t.Fatal(err)
	}
	wantListed := []platform.ListedDisk{
		{Name: "sproutfs-journal-a", Bytes: 33 << 30, Labels: east},
		{Name: "sproutfs-journal-b", Bytes: 32 << 30, Labels: east, Machines: []string{"node-a"}},
	}
	if !reflect.DeepEqual(listed, wantListed) {
		t.Fatalf("the adapter lists %+v, want %+v", listed, wantListed)
	}
	if err := disks.Delete(ctx, "sproutfs-journal-b"); !errors.Is(err, platform.ErrInUse) {
		t.Fatalf("deleting an attached disk: %v, want ErrInUse", err)
	}
	if err := disks.Delete(ctx, "projects/p/zones/z/disks/sproutfs-journal-a"); err != nil {
		t.Fatal(err)
	}
	if err := disks.Delete(ctx, "sproutfs-journal-a"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("deleting a disk the zone does not have: %v, want ErrNotFound", err)
	}
	wantCalls := []string{
		"POST disks", "POST operations/insert-sproutfs-journal-b/wait",
		"POST disks", "POST operations/insert-sproutfs-journal-a/wait",
		"POST disks", "POST operations/insert-sproutfs-journal-c/wait",
		"POST disks",
		"GET disks", "GET disks",
		"DELETE disks/sproutfs-journal-b",
		"DELETE disks/sproutfs-journal-a", "POST operations/delete-sproutfs-journal-a/wait",
		"DELETE disks/sproutfs-journal-a",
	}
	if !slices.Equal(fake.calls, wantCalls) {
		t.Fatalf("the adapter called %v, want %v", fake.calls, wantCalls)
	}
	unconfigured, err := NewGCEDisks(ctx, GCEDisksConfig{Endpoint: server.URL + "/compute/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unconfigured.List(ctx, "sproutfs-journal", "east"); !errors.Is(err, platform.ErrInvalidPath) {
		t.Fatalf("listing with no project and zone: %v, want ErrInvalidPath", err)
	}
}

// A filter names the label it lists by, and the adapter quotes its value.
func TestGCEDisksListByALabelFilter(t *testing.T) {
	fake := &fakeCompute{users: map[string]string{}, devices: map[string]string{}, sizes: map[string]int64{},
		labels: map[string]map[string]string{}}
	var filters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filters = append(filters, r.URL.Query().Get("filter"))
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	disks, err := NewGCEDisks(t.Context(), GCEDisksConfig{Project: "p", Zone: "z", Endpoint: server.URL + "/compute/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := disks.List(t.Context(), "sproutfs-journal", "east")
	if err != nil {
		t.Fatal(err)
	}
	if listed != nil {
		t.Fatalf("an empty zone lists %+v, want nothing", listed)
	}
	if want := []string{`labels.sproutfs-journal = "east"`}; !slices.Equal(filters, want) {
		t.Fatalf("the adapter sent filters %q, want %q", filters, want)
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
