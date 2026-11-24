# Diary: the box for SQL in a section of its own

The simulator page, `site/simulator/`, got a box for SQL on 2026-11-23 (`docs/diary/2026-11-23-simulator-sql-box.md`), in one grid with the log and the commit objects. Markus: "I'd like the SQL query box and results table to be in their own section below the log and commit objects on the site. It's too confusing having it all together IMO." This diary is of the builder that moves it.

## Step 1: The brief

**Author:** builder (simulator SQL section)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> 1. The SQL query box and its results get a section of their own, placed below the Log and Commit objects, visually set apart from them so that it reads as a separate thing: you watch the simulation above, you ask questions of it below.
> 2. Log and Commit objects stay together in their row and take up the space the SQL box leaves. They must look deliberate at the new widths, not stretched leftovers.
> 3. Inside the new section, lay out the query form and the results as fits the page's existing style. The results table should get the width it can use. This layout is your call; say what you chose.
> 4. Behaviour does not change: same ids, same form, same Ctrl/Cmd+Enter shortcut, same `data-pending` and status handling, same accessibility attributes. Keep the heading hierarchy sensible for the new position. Change `page.js` only if something there depends on where the section sits in the DOM.
> 5. It works at phone width (16px side gutter, no horizontal page scroll), at a middle width, and at desktop width, and in every colour scheme the page supports.
>
> Out of scope: anything else in `site/`, `README.md`, `docs/roadmap.md`, `docs/spec.md`; no new features in the SQL box. Verify with playwright at about 390, 900, and 1440 px, with a default query and a failing one; run the checks that cover `site/simulator` and `internal/simulator`, but not `make test-asan` or the long fuzz tests.

**Interpretation:** markup and CSS of `site/simulator/index.html` only: take `section#sql` out of the grid `.logs`, put it after the panel as a section of its own, and give the log and the commit objects a plain two-column row.

**Inferred intent:** the page reads top to bottom as "watch" and then "ask", so a visitor does not mistake the box for part of the log.

## Step 2: The new layout

**Author:** builder (simulator SQL section)

### What I did

- `site/simulator/index.html`, markup: `section#sql` leaves `.logs` and the panel, and becomes a section of its own in `main` after the panel, with a `.wrap` of its own. Its class is `ask` now (`sql` was also the class of a commit object's list of statements, which the old CSS kept apart only by `.logs > .sql`). Inside it: a head, `.ask-head`, with `h2#sql-h` "SQL" at the size of the other big names on the page (Requests, a, b, S3) and `#sql-to` beside it; then a framed box, `.ask-box`, with the form on the left and `.sql-out` (the status line and the rows) on the right. The ids, the form, the attributes, `data-pending`, and the text are as they were. `page.js` finds everything by id, so it did not change.
- CSS: `.ask` is a band between two rules across the whole page, like the bands of the intro and the bar, with `scroll-margin-top` for the link "Send one of your own". `.ask-box` is two fifths form and three fifths results above 960 px, and one column below, with the results under the form. `.logs` is now a plain row: the log at three fifths and the commit objects at two, with `minmax(440px, 2fr)` so that the line under a commit's bar code (name, key, instance, time, HEAD) stays on one line down to 960 px; below 960 px the two stand one above the other at 360 px each, as before. The textarea's minimum height went from 72 to 96 px, since the form column is now as tall as the results beside it.

### Why

- **Form left, results right** follows the intro's split of a narrow column and a wide one, and gives the rows the wider column: at 1440 px about 800 px. Results under the form at full width was the other choice; it puts the rows below the fold of the band at desktop width and leaves the textarea a line of 1,300 px for one statement.
- **Stacking the log and the commit objects below 960 px:** I tried them side by side at 900 px. The commit objects got 330 px and their meta line wrapped mid-item ("a" / "0:31.0"), which looked like leftovers. Stacked, each is full width, as the page had it below 960 px before.

### What didn't work

- The worktree's guard refused `playwright-cli eval` and a `run-code` whose code came through `$(cat file)`; the code went inline in double quotes, without template literals.
- After a reload with the bucket kept in `sessionStorage`, a default query 5 s into the run was "Refused by a: no valid lease: not_holder", since a has to wait out its own old lease. The screenshots clear `sessionStorage` before each load.
- Full-page screenshots drew the sticky bar in the middle of the clip; the screenshots set `.bar` to `position: static` first.

### Checks

Screenshots at 390, 900, 1000, and 1440 px, after 8 s of play, with the default query and `select missing from barcodes`; `scrollWidth - innerWidth` was 0 at each. `make site-check`: `sitecheck: ok`, no failures. `go test -tags s4ql_browser ./internal/simulator/...`: ok. The page has one colour scheme, the dark one (`color-scheme: dark`, no light tokens), so there was no other to check.

## Step 3: Self-review

**Author:** builder (simulator SQL section)

### What I did

Ran the code-review skill at low effort over `main...HEAD`; it reported no findings. Read the diff once more for selectors left from the old grid (`.logs > .log`, `.logs > .sql`, the rows of the grid); none remain. Nothing to follow up.
