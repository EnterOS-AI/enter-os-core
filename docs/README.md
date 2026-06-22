# docs/

This directory serves two purposes:

1. **Markdown content** — everything under `architecture/`, `agent-runtime/`, `api-protocol/`, `development/`, `frontend/`, `plugins/`, `product/`, etc. This is what agents and humans read.
2. **VitePress site** — `.vitepress/config.ts`, `package.json`, `package-lock.json`. These drive the rendered documentation site.

## Local preview

```bash
cd docs
npm install
npm run dev      # preview on http://localhost:5173
npm run build    # static build to docs/.vitepress/dist/
```

## Conventions

- New top-level docs must be linked from `PLAN.md`, `README.md`, and `CLAUDE.md` — otherwise agents can't find them (see `.claude/` memory `feedback_cross_reference_docs.md`).
- `edit-history/YYYY-MM-DD.md` is append-only log of significant changes; don't rewrite history.
- `archive/` holds one-shot analyses and retired docs — kept for context but not maintained.

## Why site tooling lives here (not in `docs-site/`)

VitePress expects its config at `<root>/.vitepress/config.ts` where `<root>` is also the content directory. Splitting tooling into a sibling `docs-site/` would require a non-trivial `srcDir` shim and break relative links in `.vitepress/config.ts`. Keeping both together is the pragmatic choice; this README is the tradeoff ledger.
