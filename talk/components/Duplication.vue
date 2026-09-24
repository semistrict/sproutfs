<script setup lang="ts">
import { computed } from 'vue'
import { useStep } from './Steps'

// 0 the kernel's page cache: one file per VM, one copy of the image's pages per file
// 1 and again inside each guest, when the disk is virtio-blk
// 2 the pager: one resident page per name, mapped by every VM over PMEM DAX
const step = useStep()
const vms = [0, 1, 2]
const caption = computed(() => [
  'the kernel\'s page cache is per file. Each VM\'s writable copy of the image, even a reflink, is a separate inode, so each VM gets its own copy of the same data.',
  'with virtio-blk, each guest also caches the disk in its own page cache: another copy per VM that the host cannot see.',
  'the pager keeps one resident page per name, however many VMs map it, and the guest accesses it through PMEM DAX, so host memory holds one copy.',
][step.value])
</script>

<template>
  <div>
    <svg viewBox="0 0 900 320" class="w-full">
      <!-- left: kernel -->
      <g :class="{ faded: step >= 2 }" class="fade">
        <text x="220" y="30" class="label">kernel page cache</text>
        <g v-for="v in vms" :key="v">
          <!-- guest -->
          <rect x="40" :y="50 + v * 85" width="170" height="70" rx="6" class="guest" />
          <text x="125" :y="72 + v * 85" class="small">guest {{ v + 1 }}</text>
          <g :class="{ hidden: step < 1 }" class="fade">
            <rect x="55" :y="82 + v * 85" width="140" height="28" rx="4" class="copy" />
            <text x="125" :y="101 + v * 85" class="tiny">guest page cache: copy</text>
          </g>
          <!-- host cache -->
          <rect x="240" :y="65 + v * 85" width="160" height="40" rx="4" class="copy" />
          <text x="320" :y="90 + v * 85" class="tiny">file {{ v + 1 }}: a copy</text>
          <path :d="`M 210 ${85 + v * 85} H 235`" class="link" />
        </g>
        <text x="220" y="305" class="small">{{ step >= 1 ? '2 N copies' : 'N copies' }}</text>
      </g>

      <!-- right: pager -->
      <g :class="{ hidden: step < 2 }" class="fade">
        <text x="680" y="30" class="label">the pager</text>
        <g v-for="v in vms" :key="'p' + v">
          <rect x="500" :y="50 + v * 85" width="170" height="70" rx="6" class="guest" />
          <text x="585" :y="72 + v * 85" class="small">guest {{ v + 1 }}</text>
          <text x="585" :y="98 + v * 85" class="tiny">PMEM over DAX: no cache</text>
          <path :d="`M 670 ${85 + v * 85} C 720 ${85 + v * 85}, 720 170, 750 170`" class="link shared" />
        </g>
        <rect x="750" y="150" width="130" height="40" rx="4" class="one" />
        <text x="815" y="175" class="tiny">one page, one name</text>
        <text x="680" y="305" class="small">1 copy</text>
      </g>
    </svg>
    <p class="phase">{{ caption }}</p>
  </div>
</template>

<style scoped>
.guest { fill: #1f2937; stroke: #6b7280; }
.copy { fill: #3b0f0f; stroke: #f87171; }
.one { fill: #14532d; stroke: #4ade80; }
.link { fill: none; stroke: #6b7280; stroke-width: 1.5; }
.link.shared { stroke: #4ade80; }
.label { fill: #e5e7eb; font-size: 17px; text-anchor: middle; }
.small { fill: #d1d5db; font-size: 13.5px; text-anchor: middle; }
.tiny { fill: #d1d5db; font-size: 12px; text-anchor: middle; }
.fade { transition: opacity .4s; opacity: 1; }
.hidden { opacity: 0; }
.faded { opacity: .45; }
.phase { margin-top: .25rem; font-size: 1.15rem; color: #d1d5db; min-height: 2.5rem; }
</style>
