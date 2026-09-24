<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// A timeline of one VM's writes and checkpoints.
// 0 stores land in resident pages; nothing durable between checkpoints
// 1 a checkpoint lands: everything before it is durable
// 2 the host is lost: the writes since are gone
// 3 the interval is a target; the loss window is the bound: past it, stores wait
// 4 bytes are bounded too: the dirty budget
const step = useStep()
const caption = computed(() => [
  'a store lands in a resident page: contacts nothing, cannot fail, not durable',
  'a checkpoint lands: every write before it is durable',
  'lose the host: disk writes since the last checkpoint are gone, and RAM: the VM cold boots from its disks.',
  'the interval (60 s) is a target. The loss window (5 min) bounds disk writes; an fsync waits once they are 60 s stale.',
  'bytes are bounded too: the dirty budget forces a checkpoint before it fills',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 270" class="w-full">
      <!-- time axis -->
      <line x1="40" y1="150" x2="860" y2="150" class="axis" />
      <text x="860" y="140" class="tiny right">time</text>

      <!-- writes -->
      <g v-for="i in 14" :key="i">
        <line :x1="60 + i * 50" y1="150" :x2="60 + i * 50" y2="120" class="write"
          :class="{ durable: step >= 1 && i <= 6, lost: step >= 2 && i > 6 && i <= 12, hiddenw: i > 12 }" />
        <circle :cx="60 + i * 50" cy="115" r="4" class="dot"
          :class="{ durable: step >= 1 && i <= 6, lost: step >= 2 && i > 6 && i <= 12, hiddenw: i > 12 }" />
      </g>
      <text x="200" y="100" class="small">stores → resident pages</text>

      <!-- checkpoint -->
      <g :class="{ hidden: step < 1 }" class="fade">
        <line x1="385" y1="60" x2="385" y2="200" class="ckpt" />
        <text x="385" y="50" class="small">checkpoint lands</text>
        <rect x="60" y="180" width="325" height="24" rx="4" class="band durable" />
        <text x="222" y="197" class="tiny">durable</text>
      </g>

      <!-- loss -->
      <g :class="{ hidden: step < 2 }" class="fade">
        <line x1="685" y1="60" x2="685" y2="200" class="loss" />
        <text x="685" y="50" class="small">host lost</text>
        <rect x="387" y="180" width="296" height="24" rx="4" class="band lost" />
        <text x="535" y="197" class="tiny">gone</text>
      </g>

      <!-- window -->
      <g :class="{ hidden: step < 3 }" class="fade">
        <path d="M 387 235 H 760" class="win" marker-end="url(#wh)" />
        <text x="573" y="255" class="small">loss window: 5 min</text>
        <text x="775" y="240" class="small waiting left">past it, stores wait</text>
      </g>

      <defs>
        <marker id="wh" markerWidth="8" markerHeight="8" refX="6" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="#fbbf24" />
        </marker>
      </defs>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.axis { stroke: #6b7280; stroke-width: 2; }
.write { stroke: #9ca3af; stroke-width: 2; transition: stroke .4s; }
.dot { fill: #9ca3af; transition: fill .4s; }
.write.durable, .dot.durable { stroke: #4ade80; fill: #4ade80; }
.write.lost, .dot.lost { stroke: #f87171; fill: #f87171; }
.hiddenw { opacity: 0; }
.ckpt { stroke: #4ade80; stroke-width: 2; }
.loss { stroke: #f87171; stroke-width: 2; stroke-dasharray: 6 4; }
.band.durable { fill: #14532d; }
.band.lost { fill: #3b0f0f; }
.win { stroke: #fbbf24; stroke-width: 2; fill: none; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #d1d5db; font-size: 13.5px; text-anchor: middle; }
.small.waiting { fill: #fbbf24; }
.small.left { text-anchor: start; }
.tiny { fill: #d1d5db; font-size: 12px; text-anchor: middle; }
.tiny.right { text-anchor: end; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.15rem; color: #d1d5db; min-height: 2.5rem; }
</style>
