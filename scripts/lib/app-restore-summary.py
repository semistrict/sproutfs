"""Summarise the application restore bench's rounds as markdown tables.

Reads the directory scripts/lib/app-restore-run.sh writes, one r<round>-<case>
directory per case, and prints, for each case over its rounds:

- the walk: how long the chase and the scan took, and each request's time;
- where the destination's faults read their pages from, between the snapshot
  before the restore and the one after the walk;
- the suspend, its fills of the cluster, and the restore.

Percentiles of a case are the median over its rounds of each round's
percentile, as the other GCE reports give them.

    python3 scripts/lib/app-restore-summary.py RESULTS/sproutfs-app-restore
"""
import json
import pathlib
import re
import statistics
import sys

CASES = ('cluster', 'store', 'memory')
SAMPLE = re.compile(r'^([a-zA-Z_:][a-zA-Z0-9_:]*(?:\{[^}]*\})?) (\S+)$')


def prometheus(path):
    values = {}
    for line in path.read_text().splitlines():
        match = SAMPLE.match(line)
        if match:
            values[match.group(1)] = float(match.group(2))
    return values


def percentile(values, fraction):
    ordered = sorted(values)
    return ordered[min(int(len(ordered) * fraction), len(ordered) - 1)]


class Case:
    def __init__(self, directory):
        self.dir = directory
        self.meta = json.loads((directory / 'case.json').read_text())
        self.walk = json.loads((directory / 'walk.json').read_text())
        self.load = json.loads((directory / 'load.json').read_text())

    def status(self, label, host):
        return json.loads((self.dir / f'{label}.{host}.status.json').read_text())

    def metrics(self, label, host):
        return prometheus(self.dir / f'{label}.{host}.prom')

    def cpu_seconds(self, label, host):
        for line in (self.dir / f'{label}.{host}.cpu').read_text().splitlines():
            field, value = line.split()
            if field == 'usage_usec':
                return int(value) / 1e6
        raise ValueError(f'no usage_usec in {label}.{host}.cpu')

    def hosts(self):
        return sorted(path.name.split('.')[1] for path in self.dir.glob('before.*.status.json'))

    def delta_metric(self, host, name, first='before', last='after'):
        return self.metrics(last, host).get(name, 0) - self.metrics(first, host).get(name, 0)

    def delta_status(self, host, path, first='before', last='after'):
        def get(status):
            for key in path:
                status = (status or {}).get(key, 0)
            return status or 0
        return get(self.status(last, host)) - get(self.status(first, host))

    def histogram(self, host, metric):
        """One of the destination's RAM histograms over the window, as
        (bucket upper bound, cumulative count) pairs."""
        before, after = self.metrics('before', host), self.metrics('after', host)
        buckets = []
        for name, value in after.items():
            match = re.match(metric + r'_bucket\{kind="ram",le="([^"]+)"\}', name)
            if match:
                upper = float('inf') if match.group(1) == '+Inf' else float(match.group(1))
                buckets.append((upper, value - before.get(name, 0)))
        return sorted(buckets)


def histogram_percentile(buckets, fraction):
    total = buckets[-1][1] if buckets else 0
    if total == 0:
        return 0
    for upper, cumulative in buckets:
        if cumulative >= fraction * total:
            return upper
    return float('inf')


def ms(micros):
    return f'{micros / 1000:.2f}'


def median(values):
    return statistics.median(values) if values else 0


def phase_rows(cases, phase):
    rows = []
    for name in CASES:
        runs = [c for c in cases if c.meta['case'] == name]
        if not runs:
            continue
        micros = [c.walk[phase]['micros'] for c in runs]
        rows.append([name, str(len(runs)),
                     f"{median([c.walk[phase]['seconds'] for c in runs]):.2f}",
                     f"{median([len(m) for m in micros]):.0f}",
                     ms(median([percentile(m, 0.5) for m in micros])),
                     ms(median([percentile(m, 0.9) for m in micros])),
                     ms(median([percentile(m, 0.99) for m in micros])),
                     ms(median([max(m) for m in micros])),
                     f"{median([sum(1 for v in m if v >= 1000) for m in micros]):.0f}",
                     f"{median([sum(v for v in m if v >= 1000) / 1e6 for m in micros]):.2f}"])
    return rows


def table(header, rows):
    lines = ['| ' + ' | '.join(header) + ' |', '| ' + ' | '.join('---' for _ in header) + ' |']
    lines += ['| ' + ' | '.join(row) + ' |' for row in rows]
    return '\n'.join(lines)


def main(root):
    root = pathlib.Path(root)
    # A case whose walk failed has no walk to summarise; the run's log says why.
    cases = [Case(d) for d in sorted(root.glob('r*-*'))
             if (d / 'case.json').exists() and json.loads((d / 'case.json').read_text())['walk_exit'] == 0]
    if not cases:
        sys.exit(f'no finished case under {root}')
    out = []
    first = cases[0]
    out.append(f"{len(cases)} cases. Data: {first.meta['keys']:,} keys of {first.meta['value_bytes']} bytes "
               f"and {first.meta['members']:,} members; Valkey used_memory "
               f"{int(first.load['memory']['used_memory']) / (1 << 30):.2f} GiB.\n")
    header = ['Case', 'rounds', 'seconds', 'requests', 'p50 ms', 'p90 ms', 'p99 ms', 'max ms',
              'requests >= 1 ms', 'seconds in them']
    out.append('### The chase, one dependent GET at a time\n')
    out.append(table(header, phase_rows(cases, 'chase')))
    out.append('\n### The scan, ZRANGE of 100 members at a time\n')
    out.append(table(header, phase_rows(cases, 'scan')))

    out.append('\n### Each round\n')
    rows = []
    for c in sorted(cases, key=lambda c: (c.meta['case'], c.meta['round'])):
        chase, scan = c.walk['chase']['micros'], c.walk['scan']['micros']
        rows.append([c.meta['case'], str(c.meta['round']), c.meta['source'], c.meta['destination'],
                     f"{c.meta['start_seconds']:.2f}", f"{c.walk['chase']['seconds']:.2f}",
                     ms(percentile(chase, 0.99)), f"{c.walk['scan']['seconds']:.2f}",
                     ms(percentile(scan, 0.99)), f"{c.meta['walk_exec_seconds']:.2f}"])
    out.append(table(['Case', 'round', 'suspended on', 'restored on', 'start s', 'chase s', 'chase p99 ms',
                      'scan s', 'scan p99 ms', 'exec s'], rows))

    out.append('\n### Where the destination\'s pages came from, start to end of the walk\n')
    rows = []
    for c in sorted(cases, key=lambda c: (c.meta['case'], c.meta['round'])):
        d = c.meta['destination']
        others = [h for h in c.hosts() if h != d]
        faults = c.delta_metric(d, 'sproutfs_pager_faults_total{kind="ram"}')
        loaded = c.delta_metric(d, 'sproutfs_pager_loaded_pages_total{kind="ram"}')
        fault_seconds = c.delta_metric(d, 'sproutfs_pager_fault_seconds_sum{kind="ram"}')
        buckets = c.histogram(d, 'sproutfs_pager_fault_seconds')
        requests = len(c.walk['chase']['micros']) + len(c.walk['scan']['micros'])
        served = sum(c.delta_status(h, ('cache_read', 'served_bytes')) for h in others)
        rows.append([c.meta['case'], str(c.meta['round']), f'{faults:.0f}', f'{faults / requests:.3f}',
                     f'{loaded:.0f}',
                     f'{1000 * fault_seconds / faults:.2f}' if faults else '-',
                     f'{1000 * histogram_percentile(buckets, 0.5):.2f}',
                     f'{1000 * histogram_percentile(buckets, 0.99):.2f}',
                     f"{c.delta_status(d, ('cache_memory', 'hits')):.0f}",
                     f"{c.delta_status(d, ('cache_memory', 'misses')):.0f}",
                     f"{c.delta_status(d, ('cache_disk', 'hits')):.0f}",
                     f"{c.delta_status(d, ('cache_read', 'own_hits')):.0f}",
                     f"{c.delta_status(d, ('cache_read', 'hits')):.0f}",
                     f"{c.delta_status(d, ('cache_read', 'misses')):.0f}",
                     f'{served / 1e6:.0f}',
                     f"{c.delta_status(d, ('store', 'get', 'calls')):.0f}",
                     f"{c.delta_status(d, ('store', 'get', 'bytes')) / 1e6:.0f}"])
    out.append(table(['Case', 'round', 'RAM faults', 'faults a request', 'pages loaded', 'mean fault ms',
                      'fault p50 ms', 'fault p99 ms', 'memory hits', 'memory misses', 'disk hits',
                      'own stripes', 'cluster', 'cluster misses', 'cluster MB served', 'store GETs',
                      'store MB'], rows))

    out.append('\n### The destination\'s reads of its backing, and of the cluster\n')
    rows = []
    for c in sorted(cases, key=lambda c: (c.meta['case'], c.meta['round'])):
        d = c.meta['destination']
        loads = c.delta_metric(d, 'sproutfs_pager_load_seconds_count{kind="ram"}')
        load_seconds = c.delta_metric(d, 'sproutfs_pager_load_seconds_sum{kind="ram"}')
        buckets = c.histogram(d, 'sproutfs_pager_load_seconds')
        read = [f"{c.delta_status(d, ('cache_read', key)):.0f}" for key in
                ('requests', 'replaced', 'second_requests', 'refused_by_budget', 'store_hedges',
                 'store_hedges_won', 'timeouts')]
        # The reader keeps a delay for each size of read: each class that
        # had reads, as its delay in milliseconds over its reads.
        classes = c.status('after', d).get('cache_read', {}).get('classes') or []
        delay = ', '.join(f"{k['delay'] / 1e6:.1f}/{k['reads']}" for k in classes if k.get('reads')) or '-'
        rows.append([c.meta['case'], str(c.meta['round']), f'{loads:.0f}',
                     f'{1000 * load_seconds / loads:.2f}' if loads else '-',
                     f'{1000 * histogram_percentile(buckets, 0.5):.2f}',
                     f'{1000 * histogram_percentile(buckets, 0.99):.2f}',
                     f"{c.delta_metric(d, 'sproutfs_pager_protect_traps_total{kind=\"ram\"}'):.0f}",
                     f"{c.delta_metric(d, 'sproutfs_pager_copy_on_writes_total{kind=\"ram\"}'):.0f}",
                     f"{c.delta_metric(d, 'sproutfs_pager_moved_pages_total{kind=\"ram\"}'):.0f}",
                     f"{c.delta_status(d, ('pager', 'ram', 'prefetched_pages')):.0f}",
                     f"{c.delta_status(d, ('pager', 'ram', 'prefetch_waits')):.0f}",
                     *read, delay])
    out.append(table(['Case', 'round', 'loads', 'mean load ms', 'load p50 ms', 'load p99 ms',
                      'stores into mapped pages', 'copies on write', 'pages moved',
                      'pages prefetched', 'prefetch waits',
                      'stripe requests', 'replaced', 'second requests', 'refused', 'store hedges', 'hedges won',
                      'timeouts', 'delay after, ms/reads by size'], rows))

    out.append('\n### The suspend and its fills\n')
    rows = []
    for c in sorted(cases, key=lambda c: (c.meta['case'], c.meta['round'])):
        s = c.meta['source']
        dropped = c.status('suspended', s).get('cache_fill', {}).get('dropped', {}) or {}
        dropped_before = c.status('loaded', s).get('cache_fill', {}).get('dropped', {}) or {}
        drops = {k: v - dropped_before.get(k, 0) for k, v in dropped.items() if v - dropped_before.get(k, 0)}
        kept = sum(c.delta_status(h, ('cache_fill', 'kept'), 'loaded', 'suspended') for h in c.hosts())
        uploaded = c.delta_status(s, ('store', 'put', 'bytes'), 'loaded', 'suspended') / 1e6
        # The source's processors over the suspend and its fills, which the
        # snapshot after them closes.
        busy = c.meta['suspend_seconds'] + c.meta['fills_seconds']
        cpu = (c.cpu_seconds('suspended', s) - c.cpu_seconds('loaded', s)) / busy
        rows.append([c.meta['case'], str(c.meta['round']), f"{c.meta['suspend_seconds']:.1f}",
                     f"{c.meta['fills_seconds']:.1f}", f'{uploaded:.0f}',
                     f"{uploaded / c.meta['suspend_seconds']:.1f}", f'{cpu:.2f}',
                     f"{c.delta_status(s, ('cache_fill', 'from_publications'), 'loaded', 'suspended'):.0f}",
                     f'{kept:.0f}', ', '.join(f'{k} {v}' for k, v in sorted(drops.items())) or 'none',
                     f"{c.delta_status(s, ('cache_fill', 'publication_waits'), 'loaded', 'suspended'):.0f}",
                     f"{c.delta_status(s, ('cache_fill', 'publication_waited_seconds'), 'loaded', 'suspended'):.1f}",
                     f"{c.status('suspended', s).get('cache_fill', {}).get('queued_peak_bytes', 0) / (1 << 20):.0f}"])
    out.append(table(['Case', 'round', 'suspend s', 'fills settle s', 'uploaded MB', 'uploaded MB/s',
                      'source CPUs', 'windows filled', 'stripes kept', 'stripes dropped', 'publication waits',
                      'waited s', 'queue peak MiB'], rows))

    out.append('\n### CPU over the restore and the walk, in processors\n')
    rows = []
    for c in sorted(cases, key=lambda c: (c.meta['case'], c.meta['round'])):
        wall = c.meta['start_seconds'] + c.meta['walk_exec_seconds']
        used = {h: (c.cpu_seconds('after', h) - c.cpu_seconds('before', h)) / wall for h in c.hosts()}
        d = c.meta['destination']
        rows.append([c.meta['case'], str(c.meta['round']), f'{used[d]:.2f}',
                     f'{max(v for h, v in used.items() if h != d):.2f}'])
    out.append(table(['Case', 'round', 'destination', 'busiest other host'], rows))
    print('\n'.join(out))


if __name__ == '__main__':
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
