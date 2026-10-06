<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// One window under 4+2 on six hosts.
// 0 a page's envelope (zstd + XXH3-128), and the window's first six ranks
// 1 split: four data stripes and two parity stripes, a quarter of the envelope each
// 2 fill: stripe i goes to rank i
// 3 read: k+1 = 5 of the six, picked by a hash of reader and window; any 4 rebuild it
// 4 a host is lost: its refusal is replaced at once by the rank not yet asked
// 5 and another is slow: the spare request covers it
// 6 a wrong stripe: the page's XXH3-128 fails, another set of 4 holds, its holder is told to drop it
// 7 a host joins at rank 1: every holder moves down a rank and keeps its index
const step = useStep()

const names = ['A', 'B', 'C', 'D', 'E', 'F']
const labels = ['D1', 'D2', 'D3', 'D4', 'P1', 'P2']

interface Scene { asked: string[]; used: string[]; down?: string; slow?: string; wrong?: string; refused?: string }
const scenes: Record<number, Scene> = {
  3: { asked: ['A', 'B', 'D', 'E', 'F'], used: ['A', 'B', 'D', 'E'] },
  4: { asked: ['A', 'B', 'C', 'D', 'E', 'F'], used: ['A', 'C', 'D', 'E'], down: 'B', refused: 'B' },
  5: { asked: ['A', 'C', 'D', 'E', 'F'], used: ['A', 'C', 'D', 'F'], down: 'B', slow: 'E' },
  6: { asked: ['A', 'C', 'D', 'E', 'F'], used: ['A', 'C', 'E', 'F'], down: 'B', wrong: 'D' },
  7: { asked: ['G', 'A', 'B', 'C', 'E'], used: ['A', 'B', 'C', 'E'] },
}
const scene = computed<Scene>(() => scenes[step.value] ?? { asked: [], used: [] })

// Hosts in rank order. From step 7 a new host G ranks first and holds nothing.
const hosts = computed(() => {
  const list = names.map((name, i) => ({ name, index: i }))
  return step.value >= 7 ? [{ name: 'G', index: -1 }, ...list] : list
})
const slotX = (rank: number) => 20 + rank * 122

function state(name: string) {
  const s = scene.value
  if (s.down === name) return 'down'
  if (s.wrong === name) return 'wrong'
  if (s.slow === name) return 'slow'
  if (s.used.includes(name)) return 'used'
  if (s.asked.includes(name)) return 'asked'
  return ''
}

const received = computed(() => {
  const got = new Set<number>()
  for (const h of hosts.value) if (scene.value.used.includes(h.name) && h.index >= 0) got.add(h.index)
  return got
})
const wrongIndex = computed(() => {
  const h = hosts.value.find(h => h.name === scene.value.wrong)
  return h ? h.index : -1
})

const caption = computed(() => [
  'one page of a window: its envelope is the compressed page and its XXH3-128. Rendezvous hashing ranks every cache for the window.',
  'Reed-Solomon 4+2 splits the envelope into four data stripes and two parity stripes, each a quarter of it: 1.5 times the bytes',
  'a fill sends stripe i to rank i. Each of six hosts holds one stripe; no host holds the page.',
  'a reader asks five of the six, picked by a hash of reader and window, and rebuilds from the first four that answer, whichever they are',
  'host B is lost: its refused connection marks it down, and the rank not yet asked is asked at once. Still four stripes.',
  'and host E is slow: the fifth request covers it. Two slow holders would be asked around after the p95 delay, within a budget.',
  'D answers with a wrong stripe: the rebuilt page fails its XXH3-128, another set of four holds, and D is told to drop it',
  'G joins and ranks first for this window: every holder moves down one rank and keeps its index, so a reader takes any four indices it is sent',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 360" class="w-full">
      <!-- the envelope -->
      <text x="20" y="20" class="tiny left">page envelope · 2 MiB, zstd + XXH3-128</text>
      <g v-if="step === 0">
        <rect x="20" y="28" width="480" height="34" rx="5" class="env" />
        <text x="260" y="50" class="small">envelope</text>
      </g>
      <g v-else>
        <g v-for="(l, i) in labels" :key="l">
          <rect :x="20 + i * 82" y="28" width="76" height="34" rx="5" class="stripe" :class="{ parity: i >= 4 }" />
          <text :x="58 + i * 82" y="50" class="small">{{ l }}</text>
        </g>
        <text x="520" y="42" class="tiny left">k = 4 data · m = 2 parity</text>
        <text x="520" y="58" class="tiny left">any 4 of the 6 rebuild the envelope</text>
      </g>

      <!-- the ranked hosts -->
      <g v-for="(h, r) in hosts" :key="h.name">
        <rect :x="slotX(r)" y="100" width="110" height="88" rx="8" class="host" :class="state(h.name)" />
        <text :x="slotX(r) + 55" y="120" class="tiny">rank {{ r + 1 }}</text>
        <text :x="slotX(r) + 55" y="140" class="label">host {{ h.name }}</text>
        <g v-if="step >= 2 && h.index >= 0">
          <rect :x="slotX(r) + 30" y="150" width="50" height="26" rx="4" class="stripe" :class="{ parity: h.index >= 4, bad: scene.wrong === h.name }" />
          <text :x="slotX(r) + 55" y="168" class="tiny">{{ labels[h.index] }}</text>
        </g>
        <text v-else-if="step >= 2" :x="slotX(r) + 55" y="168" class="tiny">holds nothing</text>
        <text v-if="state(h.name) === 'down'" :x="slotX(r) + 55" y="205" class="tiny warn">lost · marked down</text>
        <text v-if="state(h.name) === 'slow'" :x="slotX(r) + 55" y="205" class="tiny slowt">slow</text>
        <text v-if="state(h.name) === 'wrong'" :x="slotX(r) + 55" y="205" class="tiny warn">told to drop</text>
        <!-- the request from the reader -->
        <path v-if="step >= 3 && scene.asked.includes(h.name)" :d="`M ${slotX(r) + 55} 190 V 262`"
          class="req" :class="state(h.name)" />
      </g>

      <!-- the reader -->
      <g :class="{ hidden: step < 3 }" class="fade">
        <rect x="20" y="265" width="860" height="80" rx="8" class="reader" />
        <text x="40" y="290" class="label left">reader</text>
        <g v-for="(l, i) in labels" :key="'r' + l">
          <rect :x="140 + i * 62" y="300" width="54" height="30" rx="4" class="slot"
            :class="{ got: received.has(i), parity: received.has(i) && i >= 4, bad: wrongIndex === i }" />
          <text :x="167 + i * 62" y="320" class="tiny">{{ l }}</text>
        </g>
        <text x="540" y="312" class="small left">{{ step === 6 ? 'first four failed XXH3-128; rebuilt from another four' : 'rebuilt from four distinct indices' }}</text>
        <text x="540" y="332" class="small left ok">page checked: XXH3-128 holds · no object-store read</text>
      </g>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.env { fill: #1e3a8a; stroke: #60a5fa; }
.stripe { fill: #1e3a8a; stroke: #60a5fa; transition: fill .4s, stroke .4s; }
.stripe.parity { fill: #4c1d95; stroke: #a78bfa; }
.stripe.bad { fill: #7f1d1d; stroke: #f87171; }
.host { fill: #0f172a; stroke: #6b7280; stroke-dasharray: 6 4; transition: stroke .4s, opacity .4s; }
.host.asked { stroke: #9ca3af; stroke-dasharray: none; }
.host.used { stroke: #4ade80; stroke-dasharray: none; }
.host.slow { stroke: #fbbf24; stroke-dasharray: none; }
.host.down { stroke: #f87171; opacity: .45; }
.host.wrong { stroke: #f87171; stroke-dasharray: none; }
.req { stroke: #9ca3af; stroke-width: 2; fill: none; }
.req.used { stroke: #4ade80; }
.req.slow { stroke: #fbbf24; stroke-dasharray: 4 4; }
.req.down, .req.wrong { stroke: #f87171; stroke-dasharray: 4 4; }
.reader { fill: #111827; stroke: #4ade80; }
.slot { fill: #111827; stroke: #374151; stroke-dasharray: 3 3; transition: fill .4s, stroke .4s; }
.slot.got { fill: #1e3a8a; stroke: #60a5fa; stroke-dasharray: none; }
.slot.got.parity { fill: #4c1d95; stroke: #a78bfa; }
.slot.bad { fill: #7f1d1d; stroke: #f87171; stroke-dasharray: none; }
.label { fill: #e5e7eb; font-size: 16px; text-anchor: middle; }
.small { fill: #9ca3af; font-size: 13.5px; text-anchor: middle; }
.tiny { fill: #9ca3af; font-size: 12.5px; text-anchor: middle; }
.left { text-anchor: start; }
.warn { fill: #f87171; }
.slowt { fill: #fbbf24; }
.ok { fill: #4ade80; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.phase { margin-top: .25rem; font-size: 1.05rem; color: #d1d5db; min-height: 3rem; }
</style>
