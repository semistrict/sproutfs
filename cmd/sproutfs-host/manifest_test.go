package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/yaml"

	"github.com/semistrict/sproutfs/resource"
)

// manifests is every document of every manifest under deploy/, the probe in
// deploy/testdata included, by the file it is in.
func manifests(t *testing.T) map[string][][]byte {
	t.Helper()
	files, err := filepath.Glob("../../deploy/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	probes, err := filepath.Glob("../../deploy/testdata/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	shards, err := filepath.Glob("../../deploy/shards/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documents := map[string][][]byte{}
	for _, file := range slices.Concat(files, probes, shards) {
		text, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, document := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(text), -1) {
			documents[filepath.Base(file)] = append(documents[filepath.Base(file)], []byte(document))
		}
	}
	return documents
}

// decodeStrictly decodes one manifest document into the Kubernetes object its
// kind names, refusing any field that object does not have: a misspelt or
// misplaced field is one the API server drops or refuses, and a manifest
// that says what the cluster never sees is worse than none.
func decodeStrictly(document []byte) (any, error) {
	var header struct{ APIVersion, Kind string }
	if err := yaml.Unmarshal(document, &header); err != nil {
		return nil, err
	}
	objects := map[string]any{
		"v1/Namespace":                                    &corev1.Namespace{},
		"v1/Service":                                      &corev1.Service{},
		"v1/ServiceAccount":                               &corev1.ServiceAccount{},
		"v1/Pod":                                          &corev1.Pod{},
		"apps/v1/Deployment":                              &appsv1.Deployment{},
		"policy/v1/PodDisruptionBudget":                   &policyv1.PodDisruptionBudget{},
		"rbac.authorization.k8s.io/v1/Role":               &rbacv1.Role{},
		"rbac.authorization.k8s.io/v1/RoleBinding":        &rbacv1.RoleBinding{},
		"networking.k8s.io/v1/NetworkPolicy":              &networkingv1.NetworkPolicy{},
		"rbac.authorization.k8s.io/v1/ClusterRole":        &rbacv1.ClusterRole{},
		"rbac.authorization.k8s.io/v1/ClusterRoleBinding": &rbacv1.ClusterRoleBinding{},
		"storage.k8s.io/v1/StorageClass":                  &storagev1.StorageClass{},
		"v1/PersistentVolumeClaim":                        &corev1.PersistentVolumeClaim{},
		"v1/PersistentVolume":                             &corev1.PersistentVolume{},
	}
	object, known := objects[header.APIVersion+"/"+header.Kind]
	if !known {
		return nil, fmt.Errorf("no test knows %s %s", header.APIVersion, header.Kind)
	}
	if err := yaml.UnmarshalStrict(document, object); err != nil {
		return nil, err
	}
	return object, nil
}

// Every manifest decodes into the objects it names, with no field they do not
// have. This is the schema check deploy/ has without a cluster.
func TestEveryManifestDecodesStrictly(t *testing.T) {
	kinds := map[string][]string{}
	for file, documents := range manifests(t) {
		for _, document := range documents {
			object, err := decodeStrictly(document)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			kinds[file] = append(kinds[file], fmt.Sprintf("%T", object))
		}
	}
	want := map[string][]string{
		"00-namespace.yaml": {"*v1.Namespace"},
		"10-host.yaml":      {"*v1.Service", "*v1.Deployment", "*v1.PodDisruptionBudget"},
		"20-orchestrator.yaml": {"*v1.ServiceAccount", "*v1.Role", "*v1.ClusterRole", "*v1.ClusterRoleBinding",
			"*v1.RoleBinding", "*v1.Service", "*v1.Deployment"},
		"00-storageclass.yaml":     {"*v1.StorageClass"},
		"10-claims.yaml":           slices.Repeat([]string{"*v1.PersistentVolumeClaim"}, 6),
		"k3s-volumes.yaml":         slices.Repeat([]string{"*v1.PersistentVolume"}, 6),
		"30-networkpolicy.yaml":    {"*v1.NetworkPolicy", "*v1.NetworkPolicy"},
		"kvm-hugepages-probe.yaml": {"*v1.Pod"},
	}
	for file, objects := range want {
		if !slices.Equal(kinds[file], objects) {
			t.Fatalf("%s holds %v, want %v", file, kinds[file], objects)
		}
	}
	if len(kinds) != len(want) {
		t.Fatalf("deploy/ holds %d manifests, want %d", len(kinds), len(want))
	}
}

// hostContainer is the host container of deploy/10-host.yaml and the pod it
// runs in.
func hostContainer(t *testing.T) (corev1.PodSpec, corev1.Container) {
	t.Helper()
	for _, document := range manifests(t)["10-host.yaml"] {
		object, err := decodeStrictly(document)
		if err != nil {
			t.Fatal(err)
		}
		if deployment, ok := object.(*appsv1.Deployment); ok {
			pod := deployment.Spec.Template.Spec
			for _, container := range pod.Containers {
				if container.Name == "host" {
					return pod, container
				}
			}
		}
	}
	t.Fatal("deploy/10-host.yaml runs no host container")
	return corev1.PodSpec{}, corev1.Container{}
}

// The host manifest's environment is a configuration the host takes: what
// the ConfigMap, the Secret and the downward API supply is stood in for, and
// every literal is read as the pod reads it. Its disk is the one limiter's:
// a fifth of the filesystem free and 56 GiB used at most, a write budget, the
// default reserve, no cap of the cache's own, the cluster cache off, and no
// hot tier.
func TestTheHostManifestConfiguresAHost(t *testing.T) {
	_, container := hostContainer(t)
	supplied := map[string]string{"bucket": "sproutfs-demo-project", "prefix": "demo", "arena": "isolated",
		"token": "token", "status.podIP": "10.0.0.7", "metadata.name": "sproutfs-host-7f9c4-qk2wd",
		"metadata.namespace": "sproutfs"}
	values := map[string]string{}
	for _, variable := range container.Env {
		value := variable.Value
		if from := variable.ValueFrom; from != nil {
			switch {
			case from.ConfigMapKeyRef != nil:
				value = supplied[from.ConfigMapKeyRef.Key]
			case from.SecretKeyRef != nil:
				value = supplied[from.SecretKeyRef.Key]
			case from.FieldRef != nil:
				value = supplied[from.FieldRef.FieldPath]
			}
		}
		if _, twice := values[variable.Name]; twice {
			t.Fatalf("the host manifest sets %s twice", variable.Name)
		}
		values[variable.Name] = value
	}
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatalf("the host manifest's environment is refused: %v", err)
	}
	if want := (resource.DiskGoal{FreePercent: 20, UsedBytes: 56 << 30}); config.DiskGoal != want {
		t.Fatalf("the manifest's disk goal is %+v, want %+v", config.DiskGoal, want)
	}
	if want := (resource.WriteBudget{BytesPerDay: 1 << 40, BurstBytes: (1 << 40) / 24}); config.DiskWrites != want {
		t.Fatalf("the manifest's write budget is %+v, want %+v", config.DiskWrites, want)
	}
	if config.DiskReserveBytes != defaultDiskReserveBytes || config.DiskBandBytes != 0 {
		t.Fatalf("the manifest's reserve is %d and its band %d, want the defaults", config.DiskReserveBytes,
			config.DiskBandBytes)
	}
	if config.CacheDir != "/var/cache/sproutfs" || config.ScratchDir != "/var/lib/sproutfs" ||
		config.CacheClusterPercent != 0 {
		t.Fatalf("the manifest keeps the cache in %q beside the scratch %q with %d%% of windows in the cluster",
			config.CacheDir, config.ScratchDir, config.CacheClusterPercent)
	}
	if config.HotTier != nil {
		t.Fatalf("the manifest reads through a hot tier, %+v", *config.HotTier)
	}
	// What the host promises leaves the cache 38 GiB of the 56 it may hold:
	// 16 GiB of spill files and an ephemeral spill file of 2.
	if room := config.DiskGoal.UsedBytes - config.SpillBytes.Total() - config.Ephemeral.DiskBytes; room != 38<<30 {
		t.Fatalf("the manifest leaves the cache %d bytes under its used goal, want 38 GiB", room)
	}
}

// hostEnvironment is the host manifest's environment as a pod reads it, with
// what the ConfigMap, the Secret and the downward API supply stood in for by
// supplied.
func hostEnvironment(t *testing.T, supplied map[string]string) map[string]string {
	t.Helper()
	_, container := hostContainer(t)
	values := map[string]string{}
	for _, variable := range container.Env {
		value := variable.Value
		if from := variable.ValueFrom; from != nil {
			switch {
			case from.ConfigMapKeyRef != nil:
				value = supplied[from.ConfigMapKeyRef.Key]
			case from.SecretKeyRef != nil:
				value = supplied[from.SecretKeyRef.Key]
			case from.FieldRef != nil:
				value = supplied[from.FieldRef.FieldPath]
			}
		}
		values[variable.Name] = value
	}
	return values
}

// The same host manifest keeps the cache on shards when the ConfigMap's
// shards key says gce: the host serves the shards attached to its node, which
// it names by the downward API, through the node's /dev mounted at /host/dev.
func TestTheHostManifestServesShardsWhenTheConfigMapSaysSo(t *testing.T) {
	values := hostEnvironment(t, map[string]string{"bucket": "sproutfs-demo-project", "prefix": "demo",
		"token": "token", "status.podIP": "10.0.0.7", "metadata.name": "sproutfs-host-7f9c4-qk2wd",
		"metadata.namespace": "sproutfs", "spec.nodeName": "gke-pool-1-abcd", "shards": "gce"})
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatalf("the host manifest's environment with shards is refused: %v", err)
	}
	if config.Shards != "gce" || config.Machine != "gke-pool-1-abcd" || config.ShardDevices != "/host/dev/disk/by-id" {
		t.Fatalf("the manifest serves shards %q on %q from %q", config.Shards, config.Machine, config.ShardDevices)
	}
	pod, container := hostContainer(t)
	var mount corev1.VolumeMount
	for _, candidate := range container.VolumeMounts {
		if candidate.MountPath == "/host/dev" {
			mount = candidate
		}
	}
	directory := corev1.HostPathDirectory
	for _, volume := range pod.Volumes {
		if volume.Name == mount.Name && volume.HostPath != nil && volume.HostPath.Path == "/dev" &&
			*volume.HostPath.Type == directory {
			return
		}
	}
	t.Fatalf("the host does not mount the node's /dev at /host/dev: %+v", mount)
}

// The host's scratch is an emptyDir the pod takes with it, with no size limit
// of the kubelet's beside the disk limiter, and the page cache's directory is
// a hostPath of the node's that outlives the pod, one directory per
// namespace.
func TestTheHostManifestKeepsTheCacheOnTheNode(t *testing.T) {
	pod, container := hostContainer(t)
	volumes := map[string]corev1.VolumeSource{}
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume.VolumeSource
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.MountPath] = mount
	}
	scratch := volumes[mounts["/var/lib/sproutfs"].Name]
	if scratch.EmptyDir == nil || scratch.EmptyDir.SizeLimit != nil || scratch.EmptyDir.Medium != "" {
		t.Fatalf("the scratch is %+v, want an emptyDir on the node's disk with no size limit", scratch)
	}
	mount := mounts["/var/cache/sproutfs"]
	cache := volumes[mount.Name]
	directory := corev1.HostPathDirectoryOrCreate
	want := corev1.HostPathVolumeSource{Path: "/opt/sproutfs-demo/cache", Type: &directory}
	if cache.HostPath == nil || cache.HostPath.Path != want.Path || *cache.HostPath.Type != *want.Type ||
		mount.SubPathExpr != "$(SPROUTFS_NAMESPACE)" || mount.ReadOnly {
		t.Fatalf("the cache directory is %+v mounted as %+v, want %+v under the namespace", cache, mount, want)
	}
	if !slices.ContainsFunc(container.Env, func(variable corev1.EnvVar) bool {
		return variable.Name == "SPROUTFS_NAMESPACE" && variable.ValueFrom != nil &&
			variable.ValueFrom.FieldRef != nil && variable.ValueFrom.FieldRef.FieldPath == "metadata.namespace"
	}) {
		t.Fatal("the host container does not define the SPROUTFS_NAMESPACE its cache mount names")
	}
}
