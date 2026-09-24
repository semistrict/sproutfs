<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// Reclamation as a set difference over one VM's checkpoints.
// 0 the old root names checkpoints 3, 5, 6, 7; 5 is pinned (a fork was taken there)
// 1 checkpoint 8 lands; its root names 6, 7, 8 (3 was compacted into 8)
// 2 reclaim = named by old root − named by new root − pins = {3}; 5 stays, pinned
// 3 no collector: the pin on 5 is permanent, so 5 and everything its root references are kept
const step = useStep()
const ckpts = [3, 5, 6, 7, 8]
const oldRoot = new Set([3, 5, 6, 7])
const newRoot = new Set([6, 7, 8])
const pinned = new Set([5])
const cls = (c: number) => ({
  old: step.value >= 0 && oldRoot.has(c) && step.value < 2,
  kept: step.value >= 2 && (newRoot.has(c) || pinned.has(c)),
  dead: step.value >= 2 && !newRoot.has(c) && !pinned.has(c),
  hidden: c === 8 && step.value < 1,
})
const caption = computed(() => [
  'the VM\'s selected root names checkpoints 3, 5, 6 and 7. Checkpoint 5 is pinned: a fork was taken there.',
  'checkpoint 8 lands. Compaction rewrote the live pages of 3 into it, so its root names 6, 7 and 8.',
  'reclaim = (old root) − (new root) − (pins) = {3}. Deleted whole, index object first. 5 stays: pinned.',
  'a pin is permanent because nothing can tell when it is no longer read, so 5 and everything its root references are kept. There is no collector yet.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 300" class="w-full">
      <g v-for="(c, i) in ckpts" :key="c" :class="{ hidden: cls(c).hidden }" class="fade">
        <rect :x="60 + i * 165" y="90" width="130" height="90" rx="8" class="ckpt" :class="cls(c)" />
        <text :x="125 + i * 165" y="125" class="label">ckpt {{ c }}</text>
        <text :x="125 + i * 165" y="150" class="small">
          {{ cls(c).dead ? 'deleted' : pinned.has(c) ? 'pinned' : c === 3 ? (step >= 1 ? 'compacted into 8' : '') : '' }}
        </text>
        <text v-if="pinned.has(c)" :x="125 + i * 165" y="80" class="pin">📌 forked here</text>
      </g>
      <!-- roots -->
      <g :class="{ hidden: step >= 2 }" class="fade">
        <text x="60" y="230" class="small left">old root → 3, 5, 6, 7</text>
      </g>
      <g :class="{ hidden: step < 1 }" class="fade">
        <text x="60" y="255" class="small left">new root → 6, 7, 8</text>
      </g>
      <g :class="{ hidden: step < 2 }" class="fade">
        <text x="500" y="230" class="small left">reclaim: {3, 5, 6, 7} − {6, 7, 8} − {5} = {3}</text>
      </g>
      <g :class="{ hidden: step < 3 }" class="fade">
        <text x="500" y="255" class="small left warn">5 and everything its root references: kept</text>
      </g>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.ckpt { fill: #1f2937; stroke: #6b7280; stroke-width: 2; transition: stroke .4s, fill .4s; }
.ckpt.old { stroke: #60a5fa; }
.ckpt.kept { stroke: #4ade80; }
.ckpt.dead { stroke: #f87171; fill: #3b0f0f; stroke-dasharray: 6 4; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #d1d5db; font-size: 13.5px; text-anchor: middle; }
.small.left { text-anchor: start; }
.small.warn { fill: #fbbf24; }
.pin { fill: #fbbf24; font-size: 12px; text-anchor: middle; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.15rem; color: #d1d5db; min-height: 2.5rem; }
</style>
