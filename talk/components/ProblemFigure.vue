<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// 0 one image, many VMs: each is mostly the image
// 1 what each VM owns is a sliver; the rest is inherited
// 2 the usual operations copy the whole VM
const step = useStep()
const vms = [0, 1, 2, 3]
const caption = computed(() => [
  'many VMs, mostly the same: one image, forks of running parents, free to move between hosts',
  'a VM owns its differences. Everything else it inherited.',
  'snapshot, restore, migrate, fork: each copies the whole VM. The cost is its size — mostly bytes nobody wrote.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 320" class="w-full">
      <!-- the image -->
      <rect x="40" y="110" width="120" height="100" rx="8" class="image" />
      <text x="100" y="150" class="label">image</text>
      <text x="100" y="172" class="small">2 GiB</text>

      <!-- VMs -->
      <g v-for="v in vms" :key="v">
        <path :d="`M 165 160 C 220 160, 220 ${60 + v * 70}, 260 ${60 + v * 70}`" class="link" />
        <rect x="260" :y="40 + v * 70" width="240" height="44" rx="6" class="vm" />
        <!-- inherited part -->
        <rect x="262" :y="42 + v * 70" width="200" height="40" rx="5" class="inherited" :class="{ dim: step >= 1 }" />
        <!-- own part -->
        <rect x="462" :y="42 + v * 70" :width="36" height="40" rx="5" class="own" :class="{ lit: step >= 1 }" />
        <text x="380" :y="67 + v * 70" class="small">vm {{ v + 1 }}: {{ step >= 1 ? 'inherited' : 'the image, plus a little' }}</text>
        <text x="480" :y="67 + v * 70" class="tiny" :class="{ hidden: step < 1 }">own</text>
      </g>

      <!-- copies -->
      <g :class="{ hidden: step < 2 }" class="fade">
        <text x="690" y="24" class="label">snapshot · restore · migrate · fork</text>
        <g v-for="v in vms" :key="'c' + v">
          <path :d="`M 505 ${62 + v * 70} H 560`" class="copy" marker-end="url(#ch)" />
          <rect x="570" :y="42 + v * 70" width="240" height="40" rx="5" class="copybox" />
          <text x="690" :y="67 + v * 70" class="small">2 GiB copied · 30 MiB changed</text>
        </g>
      </g>
      <defs>
        <marker id="ch" markerWidth="8" markerHeight="8" refX="6" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="#f87171" />
        </marker>
      </defs>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.image { fill: #1e3a8a; stroke: #60a5fa; stroke-width: 2; }
.vm { fill: none; stroke: #6b7280; }
.inherited { fill: #1e3a8a; stroke: none; transition: fill .4s; }
.inherited.dim { fill: #172554; }
.own { fill: #374151; transition: fill .4s; }
.own.lit { fill: #f59e0b; }
.copybox { fill: #3b0f0f; stroke: #f87171; stroke-dasharray: 5 4; }
.link { fill: none; stroke: #60a5fa; stroke-width: 1.2; opacity: .7; }
.copy { fill: none; stroke: #f87171; stroke-width: 2; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #d1d5db; font-size: 13.5px; text-anchor: middle; }
.tiny { fill: #111827; font-size: 11px; text-anchor: middle; font-weight: 600; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.15rem; color: #d1d5db; min-height: 2.5rem; }
</style>
