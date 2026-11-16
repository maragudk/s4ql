---
version: 1
slug: "site-index-html"
primary_target: "site/index.html"
related_targets: []
---

# s4ql.com, the one-page project site

Scope: `site/index.html`, the whole site. Mode: Persuade.

Audience: Go developers curious about SQL with S3 as the only infrastructure, and people interested in building software with agents at high trust without reading the code. Markus: the experiment is about both the process and the product, so both get equal weight.

Action: follow the repository on GitHub. Nothing is usable yet; never imply it is.

Content: why it exists, what it does, status (from README "Status", dated), roadmap (from docs/roadmap.md and docs/todo.md). No invented users, benchmarks, or dates.

## Direction contract

THESIS: s4ql shown as its own data. Commit names are SHA-256, the simulation is driven by one seed; the page draws both literally instead of describing them. Refuses the dark dev-tool hero with a code block and three feature cards.

OWN-WORLD: pure black and white, no grey, no colour. Hairline bar fields, one monospace face (Commit Mono) at data density in tabular columns, full-frame inversion as the one state change. White sections are inverted frames, not cards.

STORY: s4ql is an embedded SQL database for Go that keeps everything in S3; it is written by agents and trusted only as far as tests reach; storage is built, SQL is not; follow along.

FIRST VIEWPORT: a full-width bar field drawn from a seeded sequence, its seed printed beside it. Over its left half, the two-line claim (product line, process line) at display size, the GitHub action as a framed bar button, and an EXPERIMENT status line. A column of real figures down the right edge: lease 30 s, renewal 20 s, hash SHA-256.

FORM: challenger ikeda-datamatics (Data Field), chosen over the roll after two re-rolls. seed:d870965341503b3e. Signature interaction: replay a seed, same seed same field; hover a commit barcode to read its hash and parent. Motion: the field steps in from its seed once, then holds; reduced motion shows the still.

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance
