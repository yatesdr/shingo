## The three surfaces

Verified against: ae059495

| Surface | Path | Audience | Loading model |
|---|---|---|---|
| **Core admin** | `shingo-core/www/` | Plant engineers, fleet operations | SSR + client-side enhancement, SSE for live updates |
| **Edge admin** | `shingo-edge/www/` | Plant engineers (per-cell), shift supervisors | SSR + HTMX partial swaps + per-page JS for complex forms |
| **Operator HMI** | `shingo-edge/www/static/operator-station/` | Line operators on a 10" touch panel | Empty-shell HTML + ES module JS render, SSE-driven |

These three surfaces have **intentionally different rendering models**. Do
not try to converge them. Do try to share primitives (tokens, utilities,
status vocabulary).

**The Operator HMI shares the tokens and little else.**
`operator-display.html` links `shared/tokens.css` first, then the station's own
`operator.css`, `flow-picture.css` and `composer-glyphs.css`. It loads neither
`shared/components.css` nor `shared/status-classes.css`, injects no icon
sprite, and pins `data-theme="dark"`: the HMI is a fixed dark surface with no
light theme. `operator.css` declares its own `--os-*` palette in raw hex, and
the station's rules mostly draw from that palette rather than from the shared
tokens. Where the HMI and the desktop draw the same thing (the substrate ramp,
the robot hues, the accent) its `--os-*` name is an alias of the shared token,
never a copy (see [Design tokens](04-design-tokens.md)). A new HMI rule takes a
shared token where one exists.

What the HMI is held to:

- **No emoji.** Edge's `//go:embed static/*` takes in `static/operator-station/`,
  so `TestNoEmojiInTemplatesAndPageJS` walks it.
- **The layer scale.** `TestNoRawZIndexInEdgeAssets` scans the station's files
  too, and the station's copy of the `--z-*` scale is held to the shared one by
  `TestOperatorStationZLayerScaleMatchesShared`.
- **The files both surfaces load resolve on both.** `flow-picture.css` carries
  no literal colour (`TestTheStationPictureCarriesNoLiteralColour`);
  `operator-flow.js` and `flow-picture.css` never name a bare `--os-*` token, only
  `var(--os-x, var(--shared-x))` (`TestSharedRendererNamesBothSurfacesTokens`);
  and the `--os-*` names the glyph file reads are declared by both surfaces
  (`TestBothSurfacesDeclareTheGlyphFilesTokens`).

