# vmsh website

The landing page for vmsh, NeurodeskAppX, and SquadVM.

## Local development

```bash
npm install
npm run dev
```

The committed release data keeps local development reproducible. Refresh it
from the latest published GitHub release with:

```bash
npm run release-data
```

## Builds

`npm run build` validates the site using the normal vinext build. GitHub Pages
uses `npm run build:pages` to create the static site in `out/`.
