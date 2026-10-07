"""Turn deploy/ into the application restore bench's cluster.

Reads the manifests as `kubectl create --dry-run=client -o json -f deploy/`
prints them, one object after another, and prints them as one List with the
host Deployment and the orchestrator changed for
scripts/bench-app-restore-gce.sh's nodes:

- one host pod on each node, on the node's own network, so a host's peers are
  reached over the VPC as a deployment's hosts are and not through the
  overlay's tunnel;
- a RAM arena that holds the whole guest, out of the node's HugeTLB pool, and
  the pod's memory, which a smaller node sets lower;
- the page cache's file beside the scratch on the node's local SSD;
- one template alone, at the guest's size: valkey, or what --template names;
- the cluster cache's share, and the code the orchestrator writes in the
  membership;
- the orchestrator reachable from the nodes' network, where the hosts are.

Everything else is deploy/ as it is, so the run measures the deployment.

    kubectl create --dry-run=client -o json -f deploy/ |
        python3 app-restore-manifest.py --hosts 6 --share 100 --guest-bytes 8589934592 --code 4+2 \\
            --node-cidr 10.150.0.0/20
"""
import argparse
import json
import sys

GIB = 1 << 30


def env_list(container, changes):
    """Set or remove environment variables, keeping the rest in order."""
    env = [entry for entry in container['env'] if entry['name'] not in changes]
    for name, value in changes.items():
        if value is not None:
            env.append({'name': name, 'value': str(value)})
    container['env'] = env


def host(deployment, args):
    spec = deployment['spec']
    spec['replicas'] = args.hosts
    pod = spec['template']['spec']
    pod['hostNetwork'] = True
    # The orchestrator's Service name must still resolve from the node's network.
    pod['dnsPolicy'] = 'ClusterFirstWithHostNet'
    pod['affinity'] = {'podAntiAffinity': {'requiredDuringSchedulingIgnoredDuringExecution': [{
        'labelSelector': {'matchLabels': {'app.kubernetes.io/name': 'sproutfs-host'}},
        'topologyKey': 'kubernetes.io/hostname'}]}}
    container = next(c for c in pod['containers'] if c['name'] == 'host')
    arena = args.arena_gib * GIB
    env_list(container, {
        # Nine tenths of the arena are RAM's by default, so the guest's whole
        # RAM is resident and a restore's faults are what the walk measures.
        'SPROUTFS_ARENA_BYTES': arena,
        'SPROUTFS_RAM_SHARE_PERCENT': args.ram_share,
        'SPROUTFS_MEMORY_BYTES': arena + 2 * GIB,
        # No ephemeral disks, and the pagers' own default budgets, which follow
        # the arena and the spill file.
        'SPROUTFS_EPHEMERAL_BYTES': None,
        'SPROUTFS_RAM_DIRTY_PAGES': None,
        'SPROUTFS_PMEM_DIRTY_PAGES': None,
        'SPROUTFS_TEMPLATES': f'{args.template}:{args.guest_bytes}',
        'SPROUTFS_VM_VCPUS': 2,
        'SPROUTFS_CACHE_CLUSTER_PERCENT': args.share,
        # The local SSD is 375 GB; the cache may hold most of it.
        'SPROUTFS_DISK_USED_BYTES': args.disk_used_gib * GIB,
        'GOMEMLIMIT': f'{args.memory_gib - 3}GiB',
    })
    container['resources'] = {
        'requests': {'cpu': '3', 'memory': f'{args.memory_gib}Gi', 'hugepages-2Mi': f'{args.arena_gib}Gi'},
        'limits': {'cpu': '4', 'memory': f'{args.memory_gib}Gi', 'hugepages-2Mi': f'{args.arena_gib}Gi'},
    }
    for volume in pod['volumes']:
        if volume['name'] == 'cache':
            volume['hostPath']['path'] = '/var/lib/kubelet/sproutfs-cache'


def orchestrator(deployment, args):
    pod = deployment['spec']['template']['spec']
    env_list(pod['containers'][0], {'SPROUTFS_CACHE_CODE': args.code})
    if args.orchestrator_node:
        # A bench that loses a node on purpose keeps the orchestrator off it.
        pod['nodeSelector'] = {'kubernetes.io/hostname': args.orchestrator_node}


def orchestrator_policy(policy, args):
    """Let the hosts reach the orchestrator from their nodes' network.

    A host on its node's network carries no pod's labels, so deploy/'s rule,
    which admits pods of the deployment, refuses it: the host could not ask
    it where a drain's VMs go. Its connection comes from the node's address, or from
    the overlay's address on that node once it crosses to the orchestrator's,
    so both ranges are admitted. Nothing else runs on these nodes."""
    policy['spec']['ingress'][0]['from'] += [{'ipBlock': {'cidr': args.node_cidr}},
                                             {'ipBlock': {'cidr': args.pod_cidr}}]


def read_objects(text):
    """The objects kubectl printed: one after another, or one List of them."""
    decoder, objects, at = json.JSONDecoder(), [], 0
    while True:
        while at < len(text) and text[at].isspace():
            at += 1
        if at == len(text):
            break
        found, at = decoder.raw_decode(text, at)
        objects.extend(found['items'] if found.get('kind') == 'List' else [found])
    # What kubectl fills in that no manifest says: an apply of a status it
    # read back made a second apply of the PodDisruptionBudget an update the
    # API server refuses.
    for item in objects:
        item.pop('status', None)
        item.get('metadata', {}).pop('creationTimestamp', None)
    return objects


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--hosts', type=int, required=True)
    parser.add_argument('--share', type=int, required=True)
    parser.add_argument('--guest-bytes', type=int, required=True)
    parser.add_argument('--template', default='valkey=/usr/share/sproutfs/guest/valkey.ext4',
                        help='the template, as name=path of its image in the pod')
    parser.add_argument('--arena-gib', type=int, default=10)
    parser.add_argument('--ram-share', type=int, default=90, help="the RAM arena's percent of the arena")
    parser.add_argument('--memory-gib', type=int, default=10, help="the pod's memory; Go's limit is 3 GiB less")
    parser.add_argument('--disk-used-gib', type=int, default=200, help='the most the host keeps on its disk')
    parser.add_argument('--orchestrator-node', default='', help='the node the orchestrator runs on, any by default')
    parser.add_argument('--code', required=True)
    parser.add_argument('--node-cidr', required=True)
    parser.add_argument('--pod-cidr', default='10.42.0.0/16')
    args = parser.parse_args()
    manifests = {'apiVersion': 'v1', 'kind': 'List', 'items': read_objects(sys.stdin.read())}
    changes = {('Deployment', 'sproutfs-host'): host, ('Deployment', 'sproutfs-orchestrator'): orchestrator,
               ('NetworkPolicy', 'sproutfs-orchestrator'): orchestrator_policy}
    changed = set()
    for item in manifests['items']:
        key = (item['kind'], item['metadata']['name'])
        if key in changes:
            changes[key](item, args)
            changed.add(key)
    if changed != set(changes):
        sys.exit(f'deploy/ has no {sorted(set(changes) - changed)}')
    json.dump(manifests, sys.stdout, indent=1)


if __name__ == '__main__':
    main()
