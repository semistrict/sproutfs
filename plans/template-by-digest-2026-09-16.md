# A template named by its image — 2026-09-16

## Why

A template is the VM every VM of an image is forked from: the image
imported into a root volume and checkpointed once, never run. Today each
host imports its own, named by the pod (`template-<image>-<pod>-<gen>`), so
two hosts with one image import it twice, a replaced pod imports again and
leaves its predecessor's pinned checkpoint behind for good, the identity
needs a generation to stay unique, and the pod name has to be stable, which
is the only reason the hosts are a StatefulSet.

A template is not a VM that lives on a host. It is an imported image, and an
image's identity is its bytes.

## The design

- **Identity.** `template-<sha256 of the image file>`, hex, computed by the
  host over the file it was configured with. `IsTemplate` is unchanged; the
  per-pod series of generations, `TemplateGeneration`, `PrepareTemplate`'s predecessor
  cleanup and the `Identities` listings that served it go.
- **Import if absent.** A starting host reads the template's control record.
  Present and published: it opens nothing and remembers the identity for
  `create --template`. Absent: it creates the template, imports the image
  and checkpoints it, exactly as today. Two hosts starting with the same
  image race on the record's create-if-absent: the loser reads the winner's
  record and waits for its selected checkpoint to be published, bounded by
  the import's own timeout, then proceeds; a record whose writer died before
  publishing is recovered as any VM is (the host that finds it takes the
  epoch and imports again under it), so a half-imported template does not
  wedge every host. Both cases have tests.
- **Readiness.** A host is ready when every configured image has a
  published template, whoever imported it.
- **Shape.** Memory and root size for a `create` still come from the host's
  configuration for that image name; the template's root volume size is the
  image's. The orchestrator's `memoryFor` and the table's memory column are
  unchanged.
- **Old templates.** The pinned checkpoints of superseded templates are the
  collector's, as any pinned checkpoint is. No host deletes a template: another
  host may be forking from it.
- **Deployment.** The host workload goes back to a Deployment with random
  pod names: `deploy/10-host.yaml`, the PodDisruptionBudget, the demo and
  fixes scripts that name `statefulset/sproutfs-host` or `sproutfs-host-0`,
  and the docs. `SPROUTFS_POD_NAME` stays where the host reports itself and
  drops out of template identity.
- **Check.** The `/check` allowance for superseded import epochs becomes the
  allowance for a template a later import superseded; the wording follows.

## Proof

Red first: a host test that two hosts configured with one image import it
once and both fork from it; a test that a host restarted after importing
imports nothing and is ready; a test that a changed image yields a new
template and the old one's pinned checkpoint stays; the racing-import test
and the half-imported-template test above; a simtest scenario killing a
host mid-import and restarting it, with the check clean; the fixes flow's
rollout step over a Deployment. Then `just check`, `-race` on host, volume,
simtest and cmd, 200 seeds of the topology campaign,
`TestScheduledWorldReproduces`, and the Lima Firecracker suite.

## Docs

`docs/hosting.md`, `docs/architecture.md` (the "nothing is named by content"
sentence gains its one exception and why), `deploy/README.md`, `docs/demo.md`,
`docs/open-work.md`, and a one-line note on `plans/gce-soak-2026-09-16.md`'s
template expectations.
