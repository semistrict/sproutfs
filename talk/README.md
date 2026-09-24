# The talk

This is a 45-minute walkthrough of Sproutfs for systems engineers, written as a
[Slidev](https://sli.dev) deck. `slides.md` is the deck. `components/` holds
the animated walkthroughs that the deck steps through. Each component reads the
slide's click count and draws that step.

```sh
pnpm install
pnpm dev        # serves it with hot reload and opens a browser
pnpm build      # static site under dist/
pnpm export     # PDF; `pnpm export --format png` gives one image per slide
```

In the deck, `→` advances one click and `space` advances one slide. `o` opens
the overview, and `p` opens the presenter view with notes. Every number in the
deck comes from a document under `docs/`. Every mechanism comes from
`docs/architecture.md` and the documents it links to.
