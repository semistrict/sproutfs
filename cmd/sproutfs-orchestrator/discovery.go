package main

import (
	"context"
	"fmt"
	"net/http"
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
		pods = append(pods, pod{Name: item.Name, IP: item.Status.PodIP, Ready: podReady(item)})
	}
	return pods, nil
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
