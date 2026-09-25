package vmmachine_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/vmmachine"
)

// FuzzMemoryConfigure gives Configure every configuration document a Starter
// could write. It never panics. It refuses a document that claims the managed
// memory — the managed-memory key, the RAM's size, a huge-page setting, a root
// PMEM device — or that is not the shape it adds to, and leaves a refused
// document as it was. On success the managed PMEM devices come first, and
// everything else the Starter wrote is still there, unchanged.
func FuzzMemoryConfigure(f *testing.F) {
	for _, seed := range []string{
		`null`, `{}`, `[]`,
		`{"machine-config":{"vcpu_count":2,"smt":false},"boot-source":{"kernel_image_path":"/kernel"},"drives":[]}`,
		`{"managed-memory":{"socket_path":"/elsewhere"}}`,
		`{"machine-config":{"mem_size_mib":1024}}`,
		`{"machine-config":{"huge_pages":"2M"}}`,
		`{"machine-config":"small"}`, `{"machine-config":null}`,
		`{"pmem":[{"id":"tools","path_on_host":"/tools.img","read_only":true},7,null]}`,
		`{"pmem":[{"id":"mine","path_on_host":"/root.img","root_device":true}]}`,
		`{"pmem":{"id":"tools"}}`, `{"pmem":null}`,
		`{"vsock":{"guest_cid":3,"uds_path":"/vms/vm/vsock.sock"},"pmem":[]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var document, before map[string]any
		if json.Unmarshal(raw, &document) != nil {
			return
		}
		// A second decoding is the document as the Starter wrote it, which
		// Configure cannot reach.
		if err := json.Unmarshal(raw, &before); err != nil {
			t.Fatal(err)
		}
		memory := &vmmachine.Memory{Bytes: 256 << 20, RAM: "/vms/vm/ram.sock", Pmem: []vmmachine.ManagedPmem{
			{ID: "root", Root: true, Socket: "/vms/vm/pmem-0.sock", Bytes: 2 << 20},
			{ID: "data", Socket: "/vms/vm/pmem-1.sock", Bytes: 4 << 20},
		}}
		err := memory.Configure(document)
		if want := configureRefusal(before); want != "" {
			if err == nil || err.Error() != want {
				t.Fatalf("configure %s = %v, want %s", raw, err, want)
			}
			if !reflect.DeepEqual(document, before) {
				t.Fatalf("a refused configure changed %s into %v", raw, document)
			}
			return
		}
		if err != nil {
			t.Fatalf("configure %s = %v", raw, err)
		}
		want := maps.Clone(before)
		machine := map[string]any{}
		if before["machine-config"] != nil {
			machine = maps.Clone(before["machine-config"].(map[string]any))
		}
		machine["mem_size_mib"] = uint64(256)
		want["machine-config"] = machine
		want["managed-memory"] = map[string]any{"socket_path": "/vms/vm/ram.sock"}
		pmem := []any{
			map[string]any{"id": "root", "root_device": true,
				"managed": map[string]any{"socket_path": "/vms/vm/pmem-0.sock", "length": uint64(2 << 20)}},
			map[string]any{"id": "data", "root_device": false,
				"managed": map[string]any{"socket_path": "/vms/vm/pmem-1.sock", "length": uint64(4 << 20)}},
		}
		if before["pmem"] != nil {
			pmem = append(pmem, before["pmem"].([]any)...)
		}
		want["pmem"] = pmem
		if !reflect.DeepEqual(document, want) {
			t.Fatalf("configure %s gave %v, want %v", raw, document, want)
		}
	})
}

// configureRefusal is the error Configure must give document, the first of its
// rules that it breaks, and empty for a document it accepts.
func configureRefusal(document map[string]any) string {
	if document == nil {
		return "vmmachine: there is no configuration to add the managed memory to"
	}
	if _, found := document["managed-memory"]; found {
		return "vmmachine: the configuration already names its managed memory"
	}
	if existing, found := document["machine-config"]; found {
		machine, ok := existing.(map[string]any)
		if !ok {
			return fmt.Sprintf("vmmachine: the configuration's machine-config is %T, not an object", existing)
		}
		if _, found := machine["mem_size_mib"]; found {
			return "vmmachine: the configuration already sizes the guest's memory"
		}
		if _, found := machine["huge_pages"]; found {
			return "vmmachine: managed memory takes no huge-page setting"
		}
	}
	if existing, found := document["pmem"]; found {
		devices, ok := existing.([]any)
		if !ok {
			return fmt.Sprintf("vmmachine: the configuration's pmem is %T, not a list", existing)
		}
		if slices.ContainsFunc(devices, func(device any) bool {
			object, ok := device.(map[string]any)
			return ok && object["root_device"] == true
		}) {
			return "vmmachine: the root device is the managed one"
		}
	}
	return ""
}
