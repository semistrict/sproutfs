# The talk

A 45-minute walkthrough of Sproutfs for systems engineers, as a
[Slidev](https://sli.dev) deck: `slides.md` is the deck, `components/` the
animated walkthroughs it steps through (each reads the slide's click count
and draws that step).

```sh
pnpm install
pnpm dev        # serves it with hot reload and opens a browser
pnpm build      # static site under dist/
pnpm export     # PDF; needs playwright-chromium once: pnpm add -D playwright-chromium
```

In the deck, `→` advances a click, `space` the slide; `o` is the overview and
`p` the presenter view with notes. Every number in it is from a document under
`docs/`, and every mechanism from `docs/architecture.md` and what it links.
