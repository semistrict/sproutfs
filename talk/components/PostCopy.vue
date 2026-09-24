<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// Post-copy migration.
// 0 source runs the guest; selected (7,2); pages 1,3 unpublished (dirty since)
// 1 quiesce + stop: vCPUs pause, VMM state captured; nothing sealed, nothing uploaded
// 2 hand off: regions give volumes up, keep frames; handoff = state, layout, unpublished runs, page-server address, sequence
// 3 destination opens: record read, epoch 7→8 (fence), root of (7,2) read: two objects, no page
// 4 resume: guest runs on destination; faults pull pages from source's page server first, own volume otherwise
// 5 stream: unpublished pages first, to completion; then the rest of the resident set
// 6 release: source told the destination has every unpublished page; source frees its frames
// 7 destination's next checkpoint publishes them under (8,1)
const step = useStep()
const caption = computed(() => [
  'the source runs the guest at (7,2); pages 1 and 3 written since exist only here',
  'stop: quiesce the loop, pause the vCPUs, capture the VMM state. Nothing sealed, nothing uploaded.',
  'hand off: regions give their volumes up, keep their pages, report which pages no checkpoint has',
  'the destination opens the VM: reads the record, increments the epoch 7→8, reads the root. It reads two objects and no pages, and refuses any other sequence as stale.',
  'resume: faults ask the source\'s page server first, the destination\'s own volume otherwise',
  'a stream fetches the unpublished pages first, to completion, then the rest of the resident set',
  'the destination has every unpublished page; the source is released and may exit',
  'the destination\'s next checkpoint publishes them under (8,1). The migration uploaded nothing.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 330" class="w-full">
      <!-- source -->
      <rect x="20" y="20" width="380" height="290" rx="10" class="hostbox" :class="{ released: step >= 6 }" />
      <text x="210" y="45" class="label">source host · epoch 7</text>
      <rect x="40" y="65" width="340" height="60" rx="8" class="vm" :class="{ stopped: step >= 1 }" />
      <text x="210" y="90" class="label">guest</text>
      <text x="210" y="112" class="small">{{ step === 0 ? 'running' : step < 3 ? 'stopped; VMM state captured' : step < 6 ? 'gone: fenced by epoch 8' : 'released' }}</text>
      <text x="40" y="160" class="tiny left">pages</text>
      <g v-for="p in 4" :key="'s' + p">
        <rect :x="40 + (p - 1) * 50" y="170" width="42" height="42" rx="5" class="page"
          :class="{ unpub: (p === 1 || p === 3) && step < 6, held: step >= 2 && step < 6 && (p === 1 || p === 3), freed: step >= 6 }" />
        <text :x="61 + (p - 1) * 50" y="196" class="tiny">{{ p }}</text>
      </g>
      <text x="40" y="240" class="tiny left">{{ step >= 6 ? 'pages freed' : 'pages 1, 3: no checkpoint has them' }}</text>
      <g :class="{ hidden: step < 2 || step >= 6 }" class="fade">
        <rect x="40" y="255" width="340" height="40" rx="6" class="srv" />
        <text x="210" y="280" class="small">page server: serves 1, 3 and the rest</text>
      </g>

      <!-- handoff -->
      <g :class="{ hidden: step < 2 }" class="fade">
        <rect x="405" y="115" width="90" height="100" rx="6" class="handoff" />
        <text x="450" y="140" class="tiny">handoff</text>
        <text x="450" y="158" class="tiny">state</text>
        <text x="450" y="172" class="tiny">layout</text>
        <text x="450" y="186" class="tiny">runs {1,3}</text>
        <text x="450" y="200" class="tiny">seq (7,2)</text>
      </g>

      <!-- destination -->
      <g :class="{ hidden: step < 3 }" class="fade">
        <rect x="500" y="20" width="380" height="290" rx="10" class="hostbox" />
        <text x="690" y="45" class="label">destination host · epoch 8</text>
        <rect x="520" y="65" width="340" height="60" rx="8" class="vm dst" :class="{ running: step >= 4 }" />
        <text x="690" y="90" class="label">guest</text>
        <text x="690" y="112" class="small">{{ step === 3 ? 'restored from the captured state; not yet running' : 'running' }}</text>
        <text x="520" y="160" class="tiny left">pages</text>
        <g v-for="p in 4" :key="'d' + p">
          <rect :x="520 + (p - 1) * 50" y="170" width="42" height="42" rx="5" class="page"
            :class="{
              empty: step < 4 || (step === 4 && p !== 1) || (step === 5 && p === 4),
              unpub: (p === 1 || p === 3) && step >= 4 && step < 7 && !(step === 4 && p === 3),
              own: p === 2 && step >= 5,
              streamed: p === 4 && step >= 6,
              clean: step >= 7,
            }" />
          <text :x="541 + (p - 1) * 50" y="196" class="tiny">{{ p }}</text>
        </g>
        <text x="520" y="240" class="tiny left">
          {{ step === 4 ? 'fault on page 1: fetched from the source' : step === 5 ? '3 streamed from source; 2 read from own volume' : step === 6 ? 'every unpublished page here; 4 streamed too' : step >= 7 ? 'published under (8,1): all clean' : '' }}
        </text>
        <g :class="{ hidden: step < 4 || step >= 6 }" class="fade">
          <path d="M 520 190 H 385" class="pull" marker-end="url(#ph)" />
        </g>
        <g :class="{ hidden: step < 7 }" class="fade">
          <rect x="520" y="255" width="340" height="40" rx="6" class="srv" />
          <text x="690" y="280" class="small">checkpoint (8,1): parts + index + select</text>
        </g>
      </g>
      <defs>
        <marker id="ph" markerWidth="8" markerHeight="8" refX="6" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="#fbbf24" />
        </marker>
      </defs>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.hostbox { fill: #0f172a; stroke: #6b7280; stroke-dasharray: 6 4; transition: stroke .4s; }
.hostbox.released { stroke: #374151; }
.vm { fill: #1f2937; stroke: #4ade80; stroke-width: 2; transition: stroke .4s; }
.vm.stopped { stroke: #6b7280; }
.vm.dst { stroke: #6b7280; }
.vm.dst.running { stroke: #4ade80; }
.page { fill: #374151; stroke: #6b7280; transition: fill .4s, stroke .4s; }
.page.unpub { fill: #7c2d12; stroke: #fb923c; }
.page.held { stroke: #fbbf24; }
.page.freed { fill: #111827; stroke: #374151; }
.page.empty { fill: #111827; stroke: #374151; stroke-dasharray: 3 3; }
.page.own { fill: #1e3a8a; stroke: #60a5fa; }
.page.streamed { fill: #374151; stroke: #9ca3af; }
.page.clean { fill: #374151; stroke: #6b7280; }
.srv { fill: #1f2937; stroke: #9ca3af; }
.handoff { fill: #1f2937; stroke: #fbbf24; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #9ca3af; font-size: 13.5px; text-anchor: middle; }
.tiny { fill: #9ca3af; font-size: 12.5px; text-anchor: middle; }
.tiny.left { text-anchor: start; }
.pull { fill: none; stroke: #fbbf24; stroke-width: 2; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.05rem; color: #d1d5db; min-height: 3rem; }
</style>
