# Shingo UI Style Guide

Verified against: b9210bce

The canonical reference for the Shingo UI surfaces (Core admin, Edge admin,
Operator HMI). Captures the decisions reached during the UI consistency
refactor. Subsequent changes land via PR against this document.

History: the working draft for this guide lived as `style-guide-v0.md` in
`GitHub/shingo-ui-consistency/`. It moved here when all TBD entries were
closed.

## How to use this document

Read top-to-bottom once. After that, jump to the section that covers what
you're touching. The guide is opinionated — it picks defaults rather than
listing options. When you find yourself fighting one of these conventions
on a real task, open a PR against this document, not a workaround in the
code.

The guide covers UI consistency only. It does not cover backend code style
(see [ground-rules.md](ground-rules.md)), Go architecture (design record,
outside the repo), or the operator HMI's domain-specific interaction patterns
beyond what's shared with the admin surfaces.

## How this document evolves

- Changes go via PR against `shingo/docs/ui-style-guide.md`. The
  "[deprecations tracker](ui-style-guide/33-settings-pages.md#deprecations-tracker)"
  section is the feedback loop — anything you "had
  to" do that contradicts the guide is either a deprecation candidate
  (update the guide) or a missed convention (open an issue).

This document is opinionated on purpose. When you find yourself fighting it,
update it — don't work around it.

## Contents

1. [`02-the-three-surfaces.md`](ui-style-guide/02-the-three-surfaces.md) — The three surfaces
1. [`03-code-organization.md`](ui-style-guide/03-code-organization.md) — Code organization
1. [`04-design-tokens.md`](ui-style-guide/04-design-tokens.md) — Design tokens
1. [`05-status-indicators.md`](ui-style-guide/05-status-indicators.md) — Status indicators
1. [`06-data-visualization.md`](ui-style-guide/06-data-visualization.md) — Data visualization
1. [`07-visual-principles.md`](ui-style-guide/07-visual-principles.md) — Visual principles
1. [`08-icons.md`](ui-style-guide/08-icons.md) — Icons
1. [`09-glyphs-slots-bins-and-streams.md`](ui-style-guide/09-glyphs-slots-bins-and-streams.md) — Glyphs: slots, bins and streams
1. [`10-modals.md`](ui-style-guide/10-modals.md) — Modals
1. [`11-dialog-ux-confirmation-prompt-toast.md`](ui-style-guide/11-dialog-ux-confirmation-prompt-toast.md) — Dialog UX — confirmation, prompt, toast
1. [`12-buttons.md`](ui-style-guide/12-buttons.md) — Buttons
1. [`13-forms.md`](ui-style-guide/13-forms.md) — Forms
1. [`14-placement-boxes-the-box-is-the-form.md`](ui-style-guide/14-placement-boxes-the-box-is-the-form.md) — Placement boxes — the box is the form
1. [`15-the-nodes-page-tiles-groups-sections.md`](ui-style-guide/15-the-nodes-page-tiles-groups-sections.md) — The Nodes page — tiles, groups, sections
1. [`16-desktop-composer-patterns-u9.md`](ui-style-guide/16-desktop-composer-patterns-u9.md) — Desktop composer patterns (U9)
1. [`17-javascript-primitives.md`](ui-style-guide/17-javascript-primitives.md) — JavaScript primitives
1. [`18-timestamps-plant-local-one-clock.md`](ui-style-guide/18-timestamps-plant-local-one-clock.md) — Timestamps — plant-local, one clock
1. [`19-templates-and-composition.md`](ui-style-guide/19-templates-and-composition.md) — Templates and composition
1. [`20-css-conventions.md`](ui-style-guide/20-css-conventions.md) — CSS conventions
1. [`22-tabs.md`](ui-style-guide/22-tabs.md) — Tabs
1. [`24-drift-detection.md`](ui-style-guide/24-drift-detection.md) — Drift detection
1. [`28-event-handling-delegated-actions.md`](ui-style-guide/28-event-handling-delegated-actions.md) — Event handling — delegated actions
1. [`31-mission-detail-stages.md`](ui-style-guide/31-mission-detail-stages.md) — Mission detail — stages
1. [`33-settings-pages.md`](ui-style-guide/33-settings-pages.md) — Settings pages
