<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// A fork is one pause of a running parent.
// 0 parent runs on host 1 with pages dirty since its last checkpoint (7,2)
// 1 the fork point: pause, save state, seal, resume — the parent keeps running; sequence (7,2) is pinned
// 2 two local children: control records selecting (7,2); they map the sealed frames through the pager
// 3 one remote child: its pager pulls the sealed frames from the parent's page server
// 4 each child publishes its root once it holds its inherited pages; the last hold retires the seal
const step = useStep()
const caption = computed(() => [
  'the parent runs on host 1. Its last published checkpoint is (7,2); four pages are dirty since.',
  'the fork point: pause, save VMM state, seal the dirty pages, resume — the same pause as a checkpoint\'s, but nothing is uploaded. The parent\'s record pins (7,2), once and for good.',
  'two children on the parent\'s host: each gets a record selecting a root over (7,2), and maps the sealed pages through the shared pager. A fan-out costs one pause, whatever its size.',
  'a child on another host pulls those pages out of the parent\'s page server, exactly as a migration destination does; its own volume answers everything a checkpoint holds.',
  'each child publishes its root once it holds every inherited page — that is what makes it a VM any host can open. The seal ends when the last hold retires, or at the host\'s deadline of four checkpoint intervals.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 330" class="w-full">
      <!-- host 1 -->
      <rect x="20" y="20" width="540" height="290" rx="10" class="hostbox" />
      <text x="290" y="45" class="label">host 1</text>

      <!-- parent -->
      <rect x="40" y="70" width="200" height="90" rx="8" class="vm" :class="{ sealed: step >= 1 }" />
      <text x="140" y="95" class="label">parent</text>
      <text x="140" y="118" class="small">selected (7,2)<tspan v-if="step >= 1"> · pinned (7,2)</tspan></text>
      <text x="140" y="140" class="small">{{ step >= 1 ? 'running; dirty pages sealed' : 'running; 4 dirty pages' }}</text>

      <!-- sealed frames in pager -->
      <text x="40" y="200" class="tiny left">resident pages (shared)</text>
      <g v-for="p in 4" :key="p">
        <rect :x="40 + (p - 1) * 50" y="210" width="42" height="42" rx="5" class="page" :class="{ sealed: step >= 1 && step < 4, clean: step >= 4 }" />
        <text v-if="step >= 1 && step < 4" :x="61 + (p - 1) * 50" y="205" class="lock">🔒</text>
      </g>
      <text x="40" y="285" class="tiny left">{{ step >= 4 ? 'published under the children\'s own sequences' : step >= 1 ? 'named by the fork point; shared by every child of it' : 'the parent\'s private dirty state' }}</text>

      <!-- local children -->
      <g :class="{ hidden: step < 2 }" class="fade">
        <rect x="320" y="70" width="220" height="60" rx="8" class="vm child" />
        <text x="430" y="95" class="label">child a</text>
        <text x="430" y="117" class="small">record → root over (7,2){{ step >= 4 ? ' · root published' : '' }}</text>
        <rect x="320" y="150" width="220" height="60" rx="8" class="vm child" />
        <text x="430" y="175" class="label">child b</text>
        <text x="430" y="197" class="small">record → root over (7,2){{ step >= 4 ? ' · root published' : '' }}</text>
        <path d="M 250 232 C 290 232, 290 100, 315 100" class="map" />
        <path d="M 250 232 C 290 232, 290 180, 315 180" class="map" />
        <text x="290" y="262" class="tiny">maps the sealed pages</text>
      </g>

      <!-- host 2 -->
      <g :class="{ hidden: step < 3 }" class="fade">
        <rect x="600" y="20" width="280" height="290" rx="10" class="hostbox" />
        <text x="740" y="45" class="label">host 2</text>
        <rect x="620" y="70" width="240" height="60" rx="8" class="vm child" />
        <text x="740" y="95" class="label">child c</text>
        <text x="740" y="117" class="small">record → root over (7,2){{ step >= 4 ? ' · root published' : '' }}</text>
        <text x="740" y="200" class="tiny">pager pulls sealed pages</text>
        <path d="M 560 232 H 640 L 640 140" class="map remote" marker-end="url(#fo)" />
        <text x="740" y="220" class="tiny">from host 1's page server</text>
        <text x="740" y="250" class="tiny">everything else: its own volume</text>
      </g>
      <defs>
        <marker id="fo" markerWidth="8" markerHeight="8" refX="6" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="#a78bfa" />
        </marker>
      </defs>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.hostbox { fill: #0f172a; stroke: #6b7280; stroke-dasharray: 6 4; }
.vm { fill: #1f2937; stroke: #4ade80; stroke-width: 2; transition: stroke .4s; }
.vm.sealed { stroke: #fbbf24; }
.vm.child { stroke: #a78bfa; }
.page { fill: #7c2d12; stroke: #fb923c; transition: fill .4s, stroke .4s; }
.page.sealed { fill: #78350f; stroke: #fbbf24; }
.page.clean { fill: #374151; stroke: #6b7280; }
.lock { font-size: 12.5px; text-anchor: middle; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #9ca3af; font-size: 13.5px; text-anchor: middle; }
.tiny { fill: #9ca3af; font-size: 12.5px; text-anchor: middle; }
.tiny.left { text-anchor: start; }
.map { fill: none; stroke: #a78bfa; stroke-width: 1.5; }
.map.remote { stroke-dasharray: 5 4; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.05rem; color: #d1d5db; min-height: 3rem; }
</style>
