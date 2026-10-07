package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
)

// clusterPods finds host pods through the Kubernetes API, with the
// ServiceAccount the manifests grant: get, list, watch and delete on the pods
// of one namespace, and nothing else.
type clusterPods struct {
	client    kubernetes.Interface
	namespace string
	selector  string
}

// newClusterPods authenticates with the pod's own ServiceAccount token.
func newClusterPods(namespace, selector string) (*clusterPods, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes configuration: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return &clusterPods{client: client, namespace: namespace, selector: selector}, nil
}

func (k *clusterPods) List(ctx context.Context) ([]pod, error) {
	list, err := k.client.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{LabelSelector: k.selector})
	if err != nil {
		return nil, err
	}
	pods := make([]pod, 0, len(list.Items))
	for _, item := range list.Items {
		pods = append(pods, pod{Name: item.Name, IP: item.Status.PodIP, Ready: podReady(item),
			Terminating: item.DeletionTimestamp != nil, Node: item.Spec.NodeName})
	}
	return pods, nil
}

// ShardVolumes lists the deployment's shards: the claims in the namespace
// that selector matches, each by the CSI volume handle of the persistent
// volume it is bound to, which is the name the cloud's attach API knows the
// disk by. Kubernetes provisions the disks, from a StorageClass, and never
// attaches them: no pod mounts a shard's claim. A claim not bound yet is left
// out until it is. The handles come back in order.
func (k *clusterPods) ShardVolumes(ctx context.Context, selector string) ([]string, error) {
	claims, err := k.client.CoreV1().PersistentVolumeClaims(k.namespace).List(ctx,
		metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var volumes []string
	for _, claim := range claims.Items {
		if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
			slog.WarnContext(ctx, "sproutfs-orchestrator: a shard's claim is not bound yet", "claim", claim.Name)
			continue
		}
		volume, err := k.client.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("the volume of claim %s: %w", claim.Name, err)
		}
		if volume.Spec.CSI == nil || volume.Spec.CSI.VolumeHandle == "" {
			return nil, fmt.Errorf("the volume of claim %s is not a CSI volume with a handle", claim.Name)
		}
		volumes = append(volumes, volume.Spec.CSI.VolumeHandle)
	}
	slices.Sort(volumes)
	return volumes, nil
}

// Delete removes one host pod immediately, which is the demo's host loss: no
// grace period means no preStop drain, so the VMs it ran lose everything since
// their last interval checkpoint and are recovered from that checkpoint
// elsewhere.
func (k *clusterPods) Delete(ctx context.Context, name string) error {
	grace := int64(0)
	return k.client.CoreV1().Pods(k.namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
}

// podReady reports a pod that is running, not being deleted, and says it is
// ready. A terminating pod is still listed, and still answers, which is what
// makes a drain observable.
func podReady(item corev1.Pod) bool {
	if item.DeletionTimestamp != nil || item.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range item.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// bucketRecords lists the deployment's VMs, which are the control records in
// the object namespace. A VM exists there whether or not any host runs it,
// which is exactly what a recovery needs to know.
//
// The records have a namespace of their own, holding one key per VM, so this
// listing costs a page of keys per few thousand VMs however many checkpoint
// objects they have written, and the keys alone say which VMs there are: a
// deleted VM's record is removed, so nothing here has to be read to tell a live
// VM from one that is gone.
type bucketRecords struct {
	objects platform.ObjectStore
	control *control.Client
}

func (b *bucketRecords) List(ctx context.Context) ([]listing, error) {
	ids, err := b.keys(ctx)
	if err != nil {
		return nil, err
	}
	found := make([]listing, 0, len(ids))
	for _, id := range ids {
		found = append(found, listing{ID: id})
	}
	return found, nil
}

// Pending reads one VM's record: a fork's child whose root has not landed
// selects a first checkpoint that is not created yet.
func (b *bucketRecords) Pending(ctx context.Context, id string) (bool, error) {
	record, err := b.control.Read(ctx, id)
	if errors.Is(err, platform.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !record.Created, nil
}

// Epoch reads one VM's writer epoch, zero for a VM with no record.
func (b *bucketRecords) Epoch(ctx context.Context, id string) (uint64, error) {
	record, err := b.control.Read(ctx, id)
	if errors.Is(err, platform.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return record.Epoch, nil
}

// keys is every control record in the namespace, by the identity its key names.
func (b *bucketRecords) keys(ctx context.Context) ([]string, error) {
	prefix, err := platform.NewObjectPrefix(control.RecordPrefix)
	if err != nil {
		return nil, err
	}
	var ids []string
	err = platform.ListAll(ctx, b.objects, prefix, func(object platform.ObjectMetadata) error {
		id := strings.TrimPrefix(object.Key.String(), control.RecordPrefix)
		if control.ValidID(id) {
			ids = append(ids, id)
		}
		// Anything else is not a control record: the namespace is the
		// records' own, so it is something nobody here wrote.
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// dialHost reaches one host pod's API. It is the only way the orchestrator ever
// touches a host, and every request it makes carries the deployment's token.
func dialHost(client *http.Client, port int, token string) func(pod) hostClient {
	return func(p pod) hostClient {
		return host.NewClient(fmt.Sprintf("http://%s:%d", p.IP, port), client, token)
	}
}
