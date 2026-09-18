import { computed } from 'vue'
import { useSlideContext } from '@slidev/client'

// step is the slide's current click count, which every animated component
// reads to decide what it shows. A slide declares `clicks: N` in its
// frontmatter so the navigation knows how many there are.
export function useStep() {
  const { $clicks } = useSlideContext()
  return computed(() => $clicks.value)
}
