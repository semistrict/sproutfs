<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// Fencing by epoch.
// 0 host A holds epoch 7, checkpointing (7,1), (7,2)
// 1 host A becomes unreachable; the orchestrator has positive evidence it is gone
// 2 host B opens the VM: conditional write advances the epoch to 8
// 3 host A comes back and tries to select (7,3): refused — its epoch is stale
// 4 host B's sequences are (8,1)…: above every sequence epoch 7 could allocate
const step = useStep()
const caption = computed(() => [
  'host A holds epoch 7. Its checkpoints are numbered epoch-major: (7,1), (7,2), …',
  'host A stops answering. The orchestrator takes an epoch only on positive evidence the holder is gone: its pod is no longer listed, or it answers and neither runs the VM nor serves its pages.',
  'host B opens the VM: a conditional write against the record\'s version advances the epoch to 8. That is the fence.',
  'host A comes back and tries to select (7,3). The write is refused: a stale epoch. A returns nothing to the guest but closes the VMM and releases the VM.',
  'B\'s sequences (8,1), (8,2) are above anything epoch 7 could allocate, and every checkpoint object is create-if-absent, so A\'s late uploads can collide with nothing.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 320" class="w-full">
      <!-- record -->
      <rect x="350" y="20" width="200" height="90" rx="8" class="rec" />
      <text x="450" y="45" class="label">control/&lt;id&gt;</text>
      <text x="450" y="70" class="mono">epoch <tspan class="val">{{ step >= 2 ? 8 : 7 }}</tspan></text>
      <text x="450" y="95" class="mono">selected <tspan class="val">{{ step >= 4 ? '(8,1)' : '(7,2)' }}</tspan></text>

      <!-- host A -->
      <rect x="40" y="160" width="260" height="120" rx="8" class="host" :class="{ dead: step === 1, fenced: step >= 3 }" />
      <text x="170" y="185" class="label">host A · epoch 7</text>
      <text x="170" y="210" class="small">{{ step === 1 ? 'unreachable' : step >= 3 ? 'fenced: closes the VMM, releases the VM' : 'running the guest' }}</text>
      <text x="170" y="240" class="mono">(7,1) ✓ (7,2) ✓ <tspan :class="{ hidden: step < 3 }" class="fade refused">(7,3) ✗</tspan></text>
      <text x="170" y="265" class="small" :class="{ hidden: step < 3 }">select refused: epoch 7 ≠ 8</text>

      <!-- host B -->
      <g :class="{ hidden: step < 2 }" class="fade">
        <rect x="600" y="160" width="260" height="120" rx="8" class="host b" />
        <text x="730" y="185" class="label">host B · epoch 8</text>
        <text x="730" y="210" class="small">opened at (7,2): the selected checkpoint</text>
        <text x="730" y="240" class="mono"><tspan :class="{ hidden: step < 4 }" class="fade">(8,1) ✓ (8,2) …</tspan></text>
      </g>

      <!-- arrows -->
      <path d="M 170 160 V 110 H 345" class="arrow" :class="{ refused: step >= 3 }" marker-end="url(#fh)" />
      <g :class="{ hidden: step < 2 }" class="fade">
        <path d="M 730 160 V 110 H 555" class="arrow ok" marker-end="url(#fh)" />
        <text x="640" y="100" class="tiny">IfMatch v → epoch 8</text>
      </g>
      <defs>
        <marker id="fh" markerWidth="8" markerHeight="8" refX="6" refY="4" orient="auto">
          <path d="M0,0 L8,4 L0,8 z" fill="#9ca3af" />
        </marker>
      </defs>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.rec { fill: #1f2937; stroke: #4ade80; stroke-width: 2; }
.host { fill: #1f2937; stroke: #60a5fa; stroke-width: 2; transition: stroke .4s; }
.host.dead { stroke: #6b7280; stroke-dasharray: 6 4; }
.host.fenced { stroke: #f87171; }
.host.b { stroke: #a78bfa; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #9ca3af; font-size: 13.5px; text-anchor: middle; }
.tiny { fill: #9ca3af; font-size: 12.5px; text-anchor: middle; }
.mono { fill: #9ca3af; font-size: 14px; text-anchor: middle; font-family: ui-monospace, Menlo, monospace; }
.val { fill: #e5e7eb; }
.refused { fill: #f87171; }
.arrow { fill: none; stroke: #9ca3af; stroke-width: 2; transition: stroke .4s; }
.arrow.refused { stroke: #f87171; stroke-dasharray: 6 4; }
.arrow.ok { stroke: #a78bfa; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.05rem; color: #d1d5db; min-height: 3rem; }
</style>
