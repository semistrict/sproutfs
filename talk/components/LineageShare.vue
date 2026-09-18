<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// Sharing by lineage in one pager.
// 0 parent VM has pages resident, keyed by (checkpoint, volume, page)
// 1 a child forked from the parent attaches: same identities → same frames
// 2 the child stores into page 2: a private frame, the parent's untouched
// 3 the child's checkpoint publishes it: the private frame gets an identity of its own
const step = useStep()
const pages = [0, 1, 2, 3]
const caption = computed(() => [
  'resident pages are keyed by lineage identity: (checkpoint, volume, page) — the name the volume gives every page it serves.',
  'a fork inherits its parent\'s identities, so it maps the same resident pages before its vCPUs run — nothing is copied.',
  'the child\'s first store into a page gets a private page of its own; the parent\'s is untouched.',
  'the child\'s next checkpoint publishes that page under its own sequence, which becomes that page\'s identity.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 340" class="w-full">
      <!-- arena -->
      <rect x="300" y="20" width="300" height="312" rx="10" class="arena" />
      <text x="450" y="45" class="label">pager arena (HugeTLB memfd)</text>
      <g v-for="p in pages" :key="'f' + p">
        <rect x="330" :y="60 + p * 55" width="240" height="44" rx="6" class="frame" />
        <text x="450" :y="87 + p * 55" class="small">resident page (ckpt 7, ram0, {{ p }})</text>
      </g>
      <g :class="{ hidden: step < 2 }" class="fade">
        <rect x="330" y="280" width="240" height="34" rx="6" class="frame private" :class="{ named: step >= 3 }" />
        <text x="450" y="302" class="small">{{ step >= 3 ? 'resident page (ckpt 8, ram0, 2)' : 'private page · no identity yet' }}</text>
      </g>

      <!-- parent -->
      <rect x="40" y="60" width="180" height="230" rx="8" class="vm" />
      <text x="130" y="85" class="label">parent</text>
      <g v-for="p in pages" :key="'pp' + p">
        <text x="70" :y="122 + p * 40" class="small left">page {{ p }}</text>
        <path :d="`M 120 ${117 + p * 40} C 220 ${117 + p * 40}, 250 ${82 + p * 55}, 330 ${82 + p * 55}`" class="map" />
      </g>

      <!-- child -->
      <g :class="{ hidden: step < 1 }" class="fade">
        <rect x="680" y="60" width="180" height="230" rx="8" class="vm child" />
        <text x="770" y="85" class="label">child (fork)</text>
        <g v-for="p in pages" :key="'cp' + p">
          <text x="710" :y="122 + p * 40" class="small left">page {{ p }}</text>
          <path v-if="!(p === 2 && step >= 2)"
            :d="`M 705 ${117 + p * 40} C 640 ${117 + p * 40}, 640 ${82 + p * 55}, 570 ${82 + p * 55}`" class="map child" />
          <path v-else d="M 705 197 C 640 197, 640 297, 570 297" class="map child" />
        </g>
      </g>

    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.arena { fill: #111827; stroke: #6b7280; stroke-dasharray: 6 4; }
.frame { fill: #1f2937; stroke: #9ca3af; }
.frame.private { stroke: #fb923c; fill: #3b1d0f; transition: stroke .4s, fill .4s; }
.frame.private.named { stroke: #60a5fa; fill: #1f2937; }
.vm { fill: #1f2937; stroke: #4ade80; stroke-width: 2; }
.vm.child { stroke: #a78bfa; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #9ca3af; font-size: 13.5px; text-anchor: middle; }
.small.left { text-anchor: start; }
.tiny { fill: #f87171; font-size: 14px; text-anchor: middle; }
.map { fill: none; stroke: #4ade80; stroke-width: 1.5; opacity: .8; }
.map.child { stroke: #a78bfa; }
.fade { transition: opacity .4s; opacity: 1; }
.fade.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.05rem; color: #d1d5db; min-height: 2.5rem; }
</style>
