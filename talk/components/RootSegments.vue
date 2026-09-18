<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// How a root addresses a volume's page table across checkpoints.
// 0 checkpoint 1 wrote every segment
// 1 checkpoint 2 dirtied pages in segment B only: its index carries B, its root carries A, C, D forward
// 2 checkpoint 3 changed C and D
// 3 reading a page: root → segment → page entry → (checkpoint, part, offset); segments cached by identity
const step = useStep()
const segs = ['A', 'B', 'C', 'D']
// which checkpoint's index holds each segment, per root
const roots = [
  [1, 1, 1, 1],
  [1, 2, 1, 1],
  [1, 2, 3, 3],
]
const shown = computed(() => Math.min(step.value, 2))
const caption = computed(() => [
  'the page table is cut into 512 MiB segments; the first checkpoint writes them all',
  'checkpoint 2 changed segment B only: its index carries B; its root points at checkpoint 1 for A, C, D',
  'checkpoint 3 changed C and D. Every root is complete on its own: one GET, no parent',
  'a read: root → segment → entry → (checkpoint, part, offset). Segments cache by identity. The root is ≤ 2 MiB (70 TiB of volume) and carries live-byte sums per checkpoint: liveness without a scan.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 370" class="w-full">
      <g v-for="(r, i) in roots" :key="i" :class="{ hidden: i > shown }" class="fade">
        <!-- index object of checkpoint i+1 -->
        <rect :x="40 + i * 290" y="30" width="250" height="250" rx="10" class="index" />
        <text :x="165 + i * 290" y="55" class="label">ckpt {{ i + 1 }} · index</text>
        <!-- segments carried in this index -->
        <g v-for="(s, j) in segs" :key="s">
          <rect v-if="r[j] === i + 1" :x="60 + i * 290 + j * 55" y="75" width="48" height="40" rx="5" class="seg" />
          <text v-if="r[j] === i + 1" :x="84 + i * 290 + j * 55" y="100" class="small">{{ s }}</text>
        </g>
        <!-- root -->
        <rect :x="60 + i * 290" y="135" width="210" height="130" rx="6" class="root" />
        <text :x="165 + i * 290" y="256" class="small">root</text>
        <g v-for="(s, j) in segs" :key="'r' + s">
          <text :x="80 + i * 290" :y="165 + j * 24" class="entry">{{ s }} → ckpt {{ r[j] }}</text>
          <!-- a segment an earlier checkpoint's index holds -->
          <path v-if="r[j] !== i + 1"
            :d="`M ${72 + i * 290} ${160 + j * 24} C ${30 + i * 290} ${160 + j * 24}, ${84 + (r[j] - 1) * 290 + j * 55} ${150 + j * 8}, ${84 + (r[j] - 1) * 290 + j * 55} 118`"
            class="back" />
        </g>
      </g>
      <!-- read path -->
      <g :class="{ hidden: step < 3 }" class="fade">
        <text x="870" y="335" class="tiny right">read page 700: root → segment C → its entry → (ckpt 3, part 1, offset)</text>
        <path d="M 760 322 V 268" class="read" marker-end="url(#rh)" />
        <path d="M 790 207 C 830 190, 810 150, 748 120" class="read" marker-end="url(#rh)" />
      </g>
      <defs>
        <marker id="rh" markerWidth="8" markerHeight="8" refX="6" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="#fbbf24" />
        </marker>
      </defs>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.index { fill: #111827; stroke: #60a5fa; stroke-width: 1.5; }
.seg { fill: #3b1d0f; stroke: #fb923c; }
.root { fill: #1f2937; stroke: #a78bfa; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #d1d5db; font-size: 13.5px; text-anchor: middle; }
.tiny { fill: #fbbf24; font-size: 12.5px; text-anchor: middle; }
.tiny.left { text-anchor: start; }
.tiny.right { text-anchor: end; }
.entry { fill: #d1d5db; font-size: 14px; font-family: ui-monospace, Menlo, monospace; }
.back { fill: none; stroke: #a78bfa; stroke-width: 1.2; stroke-dasharray: 4 3; opacity: .8; }
.read { fill: none; stroke: #fbbf24; stroke-width: 2; }
.fade { transition: opacity .4s; opacity: 1; }
.fade.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.05rem; color: #d1d5db; min-height: 3rem; }
</style>
