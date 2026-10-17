# Flopwire homepage

`index.html` is the production homepage. It uses the shared design tokens
in `style.css`, page styles in `homepage.css`, and interactions in `app.js`.
Fonts and agent marks are bundled under `assets`; their license and source
files are included.

## Preview

```sh
python3 -m http.server 8993 --bind 127.0.0.1 --directory landing
```

Open `/` for the production candidate. The reviewed preview remains at
`/coordination-preview/four-sections/` and carries `noindex`.

The examples use synthetic sessions. CLI output comes from an isolated
fixture; provenance is recorded in the preview README and PR #326.
The setup instruction points to `docs/quick-start-local.md` on `main`.

## Publish

The homepage workflow deploys changes under `landing/` when they merge to
`main`. It uses `wrangler.jsonc` and serves the files through `worker.js`.
The Worker redirects `www.flopwire.com` to `flopwire.com`.

Validate the production entry point, its assets, command selection, setup
copying, and desktop/mobile layout before merging. Publishing requires
all required PR checks to pass.
