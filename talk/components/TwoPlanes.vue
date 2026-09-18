<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// What a checkpoint is in the store: data objects, one index object, and the VM's one record.
// 0 the keys
// 1 a part: members, table, trailer — one suffix range GET reads the table
// 2 the index object: header, changed segments, root — one GET reads the root
// 3 the control record: epoch, nonce, selected, pins — conditional writes only
const step = useStep()
const caption = computed(() => [
  'a checkpoint is its data — the pages and the VMM state, in as many parts as they fill — and one index object, written last. A VM has one record outside that namespace.',
  'a part is a run of members — the VMM state, then pages; later, pages compaction moves out of dying checkpoints — filled to 64 MiB, closed by a table (≤ 256 KiB) and a 32-byte trailer that names it. One suffix range GET reads the table.',
  'the index object is what makes the parts a checkpoint: the page-table segments this checkpoint changed, and last the root. It is written after every part, with a create-if-absent PUT — that is the commit — and one GET of it yields the root.',
  'the control record is the only mutable object: writer epoch and nonce, the selected sequence, and the pinned sequences. Every change is a conditional write from the epoch holder.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 330" class="w-full">
      <!-- keys -->
      <g class="mono">
        <text x="30" y="40" class="key" :class="{ hot: step === 3 }">control/&lt;id&gt;</text>
        <text x="30" y="80" class="key">vm/&lt;id&gt;/ckpt/&lt;seq&gt;/</text>
        <text x="80" y="110" class="key" :class="{ hot: step === 2 }">index</text>
        <text x="80" y="140" class="key" :class="{ hot: step === 1 }">part/1</text>
        <text x="80" y="170" class="key" :class="{ hot: step === 1 }">part/2 …</text>
        <text x="30" y="215" class="key dim">vm/&lt;id&gt;/ckpt/&lt;seq+1&gt;/ …</text>
      </g>

      <!-- part anatomy -->
      <g :class="{ hidden: step !== 1 }" class="fade">
        <text x="620" y="40" class="label">part/&lt;n&gt;</text>
        <rect x="380" y="55" width="480" height="40" rx="4" class="seg state" />
        <text x="620" y="80" class="small">member: VMM state</text>
        <rect x="380" y="100" width="480" height="90" rx="4" class="seg pages" />
        <text x="620" y="150" class="small">members: pages, one 2 MiB page each</text>
        <rect x="380" y="195" width="480" height="40" rx="4" class="seg table" />
        <text x="620" y="220" class="small">table: what each member is and where it starts</text>
        <rect x="380" y="240" width="480" height="24" rx="4" class="seg trailer" />
        <text x="620" y="257" class="small">32-byte trailer: the table's offset and length</text>
        <path d="M 870 264 V 195" class="brace" />
        <text x="878" y="232" class="tiny left">← one suffix range GET</text>
      </g>

      <!-- index anatomy -->
      <g :class="{ hidden: step !== 2 }" class="fade">
        <text x="620" y="40" class="label">index</text>
        <rect x="380" y="55" width="480" height="34" rx="4" class="seg state" />
        <text x="620" y="77" class="small">fixed header</text>
        <rect x="380" y="94" width="480" height="90" rx="4" class="seg pages" />
        <text x="620" y="134" class="small">page-table segments this checkpoint changed</text>
        <text x="620" y="154" class="tiny">512 MiB of volume each = 256 page entries</text>
        <rect x="380" y="189" width="480" height="75" rx="4" class="seg root" />
        <text x="620" y="215" class="small">root: for every volume, where each segment lives</text>
        <text x="620" y="235" class="tiny">(which checkpoint's index, at what offset) — and every checkpoint it reads</text>
        <text x="620" y="253" class="tiny">complete on its own: it names no parent</text>
      </g>

      <!-- record anatomy -->
      <g :class="{ hidden: step !== 3 }" class="fade">
        <text x="620" y="40" class="label">control/&lt;id&gt;</text>
        <g class="mono">
          <text x="400" y="85" class="field">epoch      <tspan class="val">8</tspan></text>
          <text x="400" y="115" class="field">nonce      <tspan class="val">0x5f3a…</tspan></text>
          <text x="400" y="145" class="field">selected   <tspan class="val">(8 « 32) | 3</tspan>  <tspan class="tiny">epoch-major sequence</tspan></text>
          <text x="400" y="175" class="field">published  <tspan class="val">true</tspan></text>
          <text x="400" y="205" class="field">pinned     <tspan class="val">[(7 « 32) | 12]</tspan>  <tspan class="tiny">forked at; permanent</tspan></text>
        </g>
        <text x="400" y="250" class="small left">create: IfNoneMatch · update: IfMatch version · read back on a lost reply</text>
      </g>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.key { fill: #d1d5db; font-size: 18px; transition: fill .3s; }
.key.hot { fill: #fbbf24; }
.key.dim { fill: #6b7280; }
.label { fill: #e5e7eb; font-size: 18px; text-anchor: middle; }
.small { fill: #d1d5db; font-size: 13.5px; text-anchor: middle; }
.small.left { text-anchor: start; }
.tiny { fill: #9ca3af; font-size: 12.5px; text-anchor: middle; }
.tiny.left { text-anchor: start; }
.seg { stroke-width: 1.5; }
.seg.state { fill: #1e3a8a; stroke: #60a5fa; }
.seg.pages { fill: #3b1d0f; stroke: #fb923c; }
.seg.table { fill: #14532d; stroke: #4ade80; }
.seg.trailer { fill: #374151; stroke: #9ca3af; }
.seg.root { fill: #312e81; stroke: #a78bfa; }
.brace { stroke: #9ca3af; fill: none; stroke-width: 1.5; }
.field { fill: #9ca3af; font-size: 17px; }
.val { fill: #e5e7eb; }
.fade { transition: opacity .4s; opacity: 1; }
.fade.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.05rem; color: #d1d5db; min-height: 3rem; }
</style>
