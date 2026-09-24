<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// The deployment.
// 0 hosts, the object store, the orchestrator
// 1 what a host does
// 2 what the orchestrator does
// 3 Kubernetes
const step = useStep()
const caption = computed(() => [
  'two kinds of process and one object store. Every host reads and writes the store directly; hosts serve pages to each other.',
  'a host: opens VMs, checkpoints their disks on the interval, confirms its epochs, serves pages, owns the pager and the VMMs, drains before it exits.',
  'the orchestrator: identities, placement, both halves of every move and fork; surveys hosts; its table is a view, the records are the authority.',
  'Kubernetes: a Deployment of hosts, a HugeTLB pool per node, one token, one object store.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 300" class="w-full">
      <!-- object store -->
      <rect x="330" y="20" width="240" height="60" rx="8" class="store" />
      <text x="450" y="45" class="label">object store</text>
      <text x="450" y="67" class="tiny">control/ · vm/…/ckpt/…</text>

      <!-- hosts -->
      <g v-for="h in 3" :key="h">
        <rect :x="60 + (h - 1) * 280" y="150" width="220" height="100" rx="8" class="host" :class="{ lit: step === 1 }" />
        <text :x="170 + (h - 1) * 280" y="175" class="label">host {{ h }}</text>
        <text :x="170 + (h - 1) * 280" y="198" class="tiny">pager · VMMs · page server</text>
        <text :x="170 + (h - 1) * 280" y="218" class="tiny">checkpoint loop · epoch timer</text>
        <text :x="170 + (h - 1) * 280" y="238" class="tiny">fork · migrate · drain</text>
        <path :d="`M ${170 + (h - 1) * 280} 150 V 100 H 450 V 82`" class="link" />
      </g>
      <!-- pages between hosts -->
      <path d="M 280 200 H 340" class="pages" />
      <path d="M 560 200 H 620" class="pages" />
      <text x="310" y="192" class="tiny">pages</text>
      <text x="590" y="192" class="tiny">pages</text>

      <!-- orchestrator -->
      <rect x="640" y="20" width="230" height="60" rx="8" class="orch" :class="{ lit: step === 2 }" />
      <text x="755" y="45" class="label">orchestrator</text>
      <text x="755" y="67" class="tiny">identities · placement · survey · table</text>
      <path d="M 755 82 V 120 H 170 V 148" class="ctl" />
      <path d="M 755 120 H 450 V 148" class="ctl" />
      <path d="M 755 82 V 148" class="ctl" />

      <!-- kubernetes -->
      <g :class="{ hidden: step < 3 }" class="fade">
        <rect x="40" y="130" width="820" height="150" rx="10" class="k8s" />
        <text x="60" y="275" class="tiny left">Deployment of hosts · maxSurge 0 · HugeTLB pool per node · one bearer token</text>
      </g>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.store { fill: #111827; stroke: #9ca3af; stroke-dasharray: 6 4; }
.host { fill: #1f2937; stroke: #60a5fa; stroke-width: 2; transition: stroke .4s; }
.host.lit { stroke: #fbbf24; }
.orch { fill: #1f2937; stroke: #a78bfa; stroke-width: 2; transition: stroke .4s; }
.orch.lit { stroke: #fbbf24; }
.k8s { fill: none; stroke: #4ade80; stroke-dasharray: 4 4; }
.link { fill: none; stroke: #9ca3af; stroke-width: 1.5; }
.ctl { fill: none; stroke: #a78bfa; stroke-width: 1.2; opacity: .8; }
.pages { stroke: #60a5fa; stroke-width: 2; stroke-dasharray: 4 3; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.tiny { fill: #9ca3af; font-size: 12px; text-anchor: middle; }
.tiny.left { text-anchor: start; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.15rem; color: #d1d5db; min-height: 2.5rem; }
</style>
