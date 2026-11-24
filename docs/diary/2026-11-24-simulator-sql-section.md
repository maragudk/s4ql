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
