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

`npm run build` creates the local static site in `out/`. GitHub Pages uses
`npm run build:pages` to create the same site with the repository base path.
