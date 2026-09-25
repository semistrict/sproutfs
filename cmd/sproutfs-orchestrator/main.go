// Command sproutfs-orchestrator drives the demo. It is stateless: it finds the
// hosts through the Kubernetes API by the label the manifests give them, finds
// the VMs by listing control records in the bucket and asking each host what it
// runs, and carries handoffs between hosts. Nothing it knows outlives a request.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/volume"
)

// shutdownTimeout bounds the orderly stop. The orchestrator holds nothing, so
// this is only about letting the request in flight finish.
const shutdownTimeout = 30 * time.Second

// version is what this binary says it is, stamped at link time with the build's
// `git describe`.
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := run(); err != nil {
		slog.Error("sproutfs-orchestrator: exiting", "error", err)
		os.Exit(1)
	}
}

// config is the orchestrator's whole configuration, which is its environment.
type config struct {
	Bucket, Prefix, Endpoint string
	APIPort                  int
	Namespace, Selector      string
	HostAPIPort              int
	HostPagePort             int
	// TablePath is the SQLite file the VM and host tables live in, on a volume
	// that outlives the pod. Nothing in it is authority: it is rebuilt from a
	// survey and the bucket every time this process starts.
	TablePath string
}

func loadConfig(lookup func(string) string) (config, error) {
	var errs []error
	text := func(name, fallback string) string {
		if value := strings.TrimSpace(lookup(name)); value != "" {
			return value
		}
		return fallback
	}
	port := func(name string, fallback int) int {
		value := text(name, "")
		if value == "" {
			return fallback
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 || parsed > 65535 {
			errs = append(errs, fmt.Errorf("%s is %q, want a port", name, value))
			return fallback
		}
		return parsed
	}
	c := config{
		Bucket:       text("SPROUTFS_BUCKET", ""),
		Prefix:       text("SPROUTFS_PREFIX", ""),
		Endpoint:     text("SPROUTFS_GCS_ENDPOINT", ""),
		APIPort:      port("SPROUTFS_API_PORT", 8080),
		Namespace:    text("SPROUTFS_NAMESPACE", "sproutfs"),
		Selector:     text("SPROUTFS_HOST_SELECTOR", "app.kubernetes.io/name=sproutfs-host"),
		HostAPIPort:  port("SPROUTFS_HOST_API_PORT", 8080),
		HostPagePort: port("SPROUTFS_HOST_PAGE_SERVER_PORT", 8081),
		TablePath:    text("SPROUTFS_TABLE_PATH", "/var/lib/sproutfs/orchestrator.db"),
	}
	if c.Bucket == "" {
		errs = append(errs, errors.New("SPROUTFS_BUCKET is required"))
	}
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}
	return c, nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config, err := loadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	objects, client, err := adapters.NewGCS(ctx, config.Endpoint, config.Bucket, config.Prefix)
	if err != nil {
		return fmt.Errorf("gcs object store: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			slog.Error("sproutfs-orchestrator: closing the object store client", "error", err)
		}
	}()
	pods, err := newClusterPods(config.Namespace, config.Selector)
	if err != nil {
		return err
	}
	// The listing reads each record it finds, because whether a VM is deleted
	// is written in the record rather than in its key.
	records, err := control.NewClient(control.Config{ObjectStore: objects})
	if err != nil {
		return fmt.Errorf("control records: %w", err)
	}
	// A migration's receive returns only once the destination holds every page
	// the source's checkpoints do not, so the host client waits as long as a
	// guest's memory takes to cross the network. Nothing else waits that long:
	// a survey gives each host's status a deadline of its own, so a host that
	// has stopped answering costs a request seconds rather than this.
	hosts := &http.Client{Timeout: 10 * time.Minute}
	// One token admits the whole control plane: this orchestrator to the hosts,
	// a draining host to this API, and the CLI to it.
	token := jsonhttp.Token(os.Getenv(jsonhttp.TokenEnv))
	if token == "" {
		slog.Warn("sproutfs-orchestrator: serving without authentication",
			"set", jsonhttp.TokenEnv)
	}
	catalog, err := openTable(ctx, config.TablePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := catalog.Close(); err != nil {
			slog.Error("sproutfs-orchestrator: closing the VM table", "error", err)
		}
	}()
	o := &orchestrator{
		pods: pods, records: &bucketRecords{objects: objects, control: records},
		dial: dialHost(hosts, config.HostAPIPort, token), identify: newIdentity,
		apiPort: config.HostAPIPort, pagePort: config.HostPagePort, table: catalog,
		audit: auditing(objects),
	}
	// The table is rebuilt from the deployment itself before anything is
	// served — a file left by a previous process describes a cluster that has
	// moved on — and on its own timer after that, which is what releases a
	// handover nothing is waiting on and forgets a VM another orchestrator
	// deleted. A request never does it: reconciling reads every control record
	// in the bucket and writes the whole table, and a console polled at a few
	// hertz would make this process's only SQLite writer the busiest thing in
	// the deployment.
	if err := o.Reconcile(ctx); err != nil {
		slog.Warn("sproutfs-orchestrator: rebuilding the VM table at startup failed", "error", err)
	}
	go o.Reconciling(ctx, 0)
	server := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(config.APIPort)),
		Handler:           newServer(o, token),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	failed := make(chan error, 1)
	go func() {
		slog.Info("sproutfs-orchestrator: serving", "version", version, "address", server.Addr,
			"namespace", config.Namespace, "selector", config.Selector, "bucket", config.Bucket)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
			return
		}
		failed <- nil
	}()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

// auditing is the deployment check this orchestrator answers GET /check with:
// volume.CheckDeployment over the whole object namespace, with the leftovers a
// live deployment always has allowed by name.
//
// The store is already scoped to the deployment's prefix, so the check is given
// none of its own: everything it lists is under that prefix and nothing below
// it carries the prefix a second time.
//
// Every allowance here is something no writer ever comes back for, and each is
// the ordinary state of a deployment that is running rather than one that has
// quiesced:
//
//   - a publication that has not reached its index is one in flight while the
//     check read the bucket, which is every checkpoint being taken right now;
//   - objects under a VM with no control record are the checkpoints a deleted
//     VM pinned for its descendants, which is what every deleted VM that was
//     ever forked leaves;
//   - a checkpoint that is neither selected nor pinned is a VM's own root, and
//     the intermediate checkpoints a guest image's import publishes on its way
//     into a template;
//   - a superseded epoch's checkpoints are what every takeover of a lost VM
//     leaves behind it, a template whose unfinished import a later one
//     recovered among them: a template is named by its image's bytes, so a host
//     that finds one half imported takes the epoch and imports again under it
//     rather than choosing another name.
//
// Nothing else is excusable, and a violation with no allowance is durable state
// disagreeing with itself: an index naming a part that is not there, a pinned
// checkpoint that does not open, an object under no VM at all.
func auditing(objects platform.ObjectStore) func(context.Context) error {
	return func(ctx context.Context) error {
		return volume.CheckDeployment(ctx, objects, platform.ObjectPrefix{},
			volume.AllowUnpublishedIndex, volume.AllowUnrecordedVM,
			volume.AllowUnreferencedCheckpoint, volume.AllowSupersededEpoch)
	}
}

// newIdentity allocates a VM identity. A ULID is sortable by the moment it was
// made, which is what makes a listing of a demo's VMs read in the order they
// were created, and it needs nothing to coordinate with.
func newIdentity() string {
	return "vm-" + strings.ToLower(ulid.Make().String())
}
