<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// The checkpoint's pause, then what runs behind the guest.
// 0 guest running, dirty pages accumulating
// 1 pause: vCPUs stop
// 2 VMM state saved
// 3 seal: dirty pages write-protected in place
// 4 resume: guest runs; a store into a sealed page copies that one page
// 5 parts upload behind the guest
// 6 index object PUT (create-if-absent): the commit
// 7 control record selects it: durable
const step = useStep()
const pages = [0, 1, 2, 3, 4, 5, 6, 7]
const dirty = [1, 2, 5, 6]
const stored = 5 // the page the guest stores into after the seal
const phase = computed(() => [
  'guest running; four dirty pages',
  'pause: vCPUs stop',
  'save the VMM state — a capture only; the interval seals the disks alone',
  'seal: write-protect the dirty pages in place — nothing copied',
  'resume; a store into a sealed page copies that one page',
  'parts upload behind the running guest',
  'index object last: the checkpoint is published',
  'the record selects it: the VM survives losing this host',
][step.value])
const paused = computed(() => step.value >= 1 && step.value < 4)
</script>

<template>
  <div class="ci">
    <svg viewBox="0 0 900 360" class="w-full">
      <!-- guest -->
      <rect x="40" y="30" width="240" height="60" rx="8" class="box" :class="{ paused }" />
      <text x="160" y="55" class="label">guest vCPUs</text>
      <text x="160" y="77" class="small">{{ paused ? 'paused' : 'running' }}</text>

      <!-- VMM state -->
      <g :class="{ hidden: step < 2 }" class="fade">
        <rect x="300" y="30" width="150" height="60" rx="8" class="box state" />
        <text x="375" y="55" class="label">VMM state</text>
        <text x="375" y="77" class="small">registers, devices</text>
      </g>

      <!-- pager frames -->
      <text x="40" y="135" class="small left">resident pages in the pager (2 MiB each)</text>
      <g v-for="p in pages" :key="p">
        <rect :x="40 + p * 56" y="150" width="48" height="48" rx="6" class="page"
          :class="{
            dirty: dirty.includes(p) && (step < 3 || (step >= 3 && step < 7)),
            sealed: dirty.includes(p) && step >= 3 && step < 7,
            clean: !dirty.includes(p) || step >= 7,
          }" />
        <text :x="64 + p * 56" y="180" class="tiny">{{ p }}</text>
        <g v-if="dirty.includes(p) && step >= 3 && step < 7">
          <rect :x="52 + p * 56" y="140" width="24" height="16" rx="3" class="lock" />
          <text :x="64 + p * 56" y="152" class="tiny lock-t">🔒</text>
        </g>
      </g>
      <!-- the private copy the resumed guest's store makes -->
      <g :class="{ hidden: step < 4 }" class="fade">
        <rect :x="40 + stored * 56" y="215" width="48" height="48" rx="6" class="page dirty" />
        <text :x="64 + stored * 56" y="245" class="tiny">5′</text>
        <path :d="`M ${64 + stored * 56} 200 v 12`" class="arrow" marker-end="url(#ah)" />
        <text :x="64 + stored * 56 + 40" y="245" class="small left">copy on write: one page</text>
      </g>

      <!-- object store -->
      <rect x="560" y="120" width="300" height="200" rx="10" class="store" />
      <text x="710" y="145" class="label">object store</text>
      <g :class="{ hidden: step < 5 }" class="fade">
        <rect x="580" y="165" width="120" height="34" rx="6" class="obj" />
        <text x="640" y="187" class="small">part/1</text>
        <rect x="710" y="165" width="120" height="34" rx="6" class="obj" />
        <text x="770" y="187" class="small">part/2</text>
      </g>
      <g :class="{ hidden: step < 6 }" class="fade">
        <rect x="580" y="215" width="250" height="34" rx="6" class="obj index" />
        <text x="705" y="237" class="small">index: page tables + root</text>
      </g>
      <g :class="{ hidden: step < 7 }" class="fade">
        <rect x="580" y="265" width="250" height="34" rx="6" class="obj record" />
        <text x="705" y="287" class="small">control/&lt;id&gt; → selected = seq</text>
      </g>

      <!-- upload arrows -->
      <g :class="{ hidden: step < 5 }" class="fade">
        <path d="M 490 175 H 550" class="arrow" marker-end="url(#ah)" />
        <text x="520" y="165" class="tiny">upload</text>
      </g>

      <defs>
        <marker id="ah" markerWidth="8" markerHeight="8" refX="6" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="#9ca3af" />
        </marker>
      </defs>
    </svg>
    <p class="phase">{{ phase }}</p>
  </div>
</template>

<style scoped>
.ci { font-family: inherit; }
.box { fill: #1f2937; stroke: #4ade80; stroke-width: 2; transition: stroke .3s; }
.box.paused { stroke: #f59e0b; }
.box.state { stroke: #60a5fa; }
.label { fill: #e5e7eb; font-size: 18px; text-anchor: middle; }
.small { fill: #9ca3af; font-size: 13.5px; text-anchor: middle; }
.small.left, .tiny.left { text-anchor: start; }
.tiny { fill: #9ca3af; font-size: 12.5px; text-anchor: middle; }
.page { fill: #374151; stroke: #6b7280; stroke-width: 1.5; transition: fill .4s, stroke .4s; }
.page.dirty { fill: #7c2d12; stroke: #fb923c; }
.page.sealed { fill: #78350f; stroke: #fbbf24; }
.page.clean { fill: #374151; stroke: #6b7280; }
.lock { fill: #fbbf24; }
.lock-t { font-size: 10px; }
.store { fill: #111827; stroke: #6b7280; stroke-dasharray: 6 4; }
.obj { fill: #1f2937; stroke: #9ca3af; }
.obj.index { stroke: #60a5fa; }
.obj.record { stroke: #4ade80; }
.arrow { stroke: #9ca3af; stroke-width: 2; fill: none; }
.fade { transition: opacity .4s; opacity: 1; }
.fade.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.05rem; color: #d1d5db; min-height: 2.5rem; }
</style>
