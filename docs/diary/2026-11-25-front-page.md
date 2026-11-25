# Diary: a shorter front page

The public front page, `/site/index.html` (www.s4ql.com), had grown into a detailed status report, with a roadmap of 26 rows and a Status section. Markus wants it to present the project to a new user instead. Agreed with him: the roadmap becomes a few milestones with a link to the full roadmap on GitHub, the Status section goes, and the `h1` stays, so `/site/og.png` stays too.

## Step 1: the writer's draft

**Author:** builder (recorded from the lead's brief; the writer did the work)

### Prompt Context

**Verbatim prompt:** Markus, to the lead: "make the front page more concise and summarize some information. In particular the roadmap is getting way too long. We want to present the project properly to a new user, not give a detailed status."
**Interpretation:** cut the page's text, fold the roadmap into a few milestones with a link to the full one, remove the Status section, and keep the headline.
**Inferred intent:** a newcomer should understand what s4ql is and where it is going in one read, while the detail stays in `/docs/roadmap.md` and the README.

### What I did

A writer sub-agent drafted the rework, and the builder committed its file unchanged as `ec219f7`. The draft takes the page text from about 2,800 to 1,200 words and the roadmap from 26 rows to 7. It removes the Status section and its CSS (`.status-table`). It moves the roadmap into the inverted (white) band, changes the colour of the roadmap glyphs from `--lumin` to `--ink` so they show on white, removes the roadmap's legend, and labels the rows Built, Next, and Later. A button links to the full roadmap on GitHub. The script is unchanged.

### Why

The roadmap and the status had become a changelog. A few milestones say where the project is without asking a newcomer to read slice by slice.

### What worked

The inverted band sets the roadmap apart without a legend: the filled bar code, the open bars, and the dashed box read as built, next, and later together with the label at the right.

### What didn't work

Nothing failed in this step as far as the builder knows. The lead then had the draft reviewed by Gemini as a second opinion and checked its claims against the repository, which gave the list of changes in step 2.

### What I learned

The roadmap's glyphs used `--lumin` directly, so they vanish in the inverted band; `--ink` follows the band.

### What was tricky

Keeping built progress visible, which Markus values, while cutting the rows to a handful.

### What warrants review

The review found statements in the draft that overstate or understate what is built; they are in step 2.

### Future work

None beyond step 2.

## Step 2: corrections after the second opinion

**Author:** builder

### Prompt Context

**Verbatim prompt:** "You finish a rework of s4ql's public front page, `site/index.html` (www.s4ql.com). Read `AGENTS.md` first; its rules bind you, especially \"On the site, design before words\". House style for page text: plain, short sentences, no em dashes." The brief then lists nine changes, from the hero's lede to the row "Trust without reading", which "What I did" below follows one by one.
**Interpretation:** apply the lead's checked list of corrections to the draft, verify each fact against the files the brief names, and check the page in a browser at 1440 and 390 pixels wide.
**Inferred intent:** the short page must stay true. Every sentence a newcomer reads must match the code and the docs.

### What I did

All changes are in `/site/index.html`:

1. The hero's lede now says that writes commit to S3 and reads come from a local SQLite cache, so a reader does not assume every query goes to S3.
2. The hero's rail drops "Written by" and "Tested with", which the `h1` already says, for "Reads: Local SQLite cache" and "Interface: database/sql (cgo)". `/docs/spec.md`: "The module therefore needs cgo."
3. The chip "First SQL built" became "SQL on one table built".
4. The roadmap row "SQL front end" is one sentence: a tokenizer and a parser of s4ql's own for all of SQLite's grammar, checked against SQLite on the statements of its own tests. The draft claimed agreement on the shape of the tree and on every syntax error message, which `/internal/sql/parser/README.md` ("The tree has the shape of the text, not the shape of SQLite's internal form") and the known gaps in `/docs/todo/sql-front-end.md` contradict. `/internal/sql/corpus/README.md` confirms the corpus is from SQLite's own test scripts, documentation, and fuzz corpora.
5. The row "SQL on one table" has the label Built, with "Built in 15 slices." at the end of its text. It says `strict, without rowid`, and that the scalar subquery may read another table and stands in the `where` of `select`, `update`, and `delete` (`/docs/roadmap.md`, slice 15).
6. The Next rows speak to a user: "Expressions" is operators and functions, each giving SQLite's answer or an error; "Grouping and joining" is `group by`, aggregates, and `join`. The memory limit has a row of its own, "A memory limit", worded for any statement, because `/docs/todo/operators.md` applies it to every operator that holds rows, the rows of a `returning` included. "More ways to write" is now "Upserts and batches".
7. A Later row "More SQL": more of SQLite's SQL piece by piece, and large intermediate results spooled to the cache.
8. In "What it does": "Reads" and "Cache" are one row; the lease row says a spare takes over "when the lease is released or times out" (`/docs/spec.md`); "S3 must offer" ends "as Amazon S3 and Tigris do" (both named in the doc comment of `s4ql.Options.Endpoint` in `/s4ql.go`); "Interface" shows `s4ql.Open(ctx, s4ql.Options{Bucket: "app", Prefix: "users/42", CacheDir: "cache"})` returning a standard `*sql.DB`. `Bucket` and `CacheDir` are the two fields that `/s4ql.go` calls required.
9. "Trust without reading" drops "AI agents write and own the code.", which the hero says.

The roadmap now has nine rows: three Built, four Next, two Later. The page text is 1,181 words, counted as the words of the `innerText` of the header, `main`, and the footer.

I served `/site/` with `python3 -m http.server` and checked it with playwright-cli at 1440 by 900 and 390 by 844: `scrollWidth` equals `clientWidth` at both, no element of `main` reaches past the viewport, and the console has no messages. A script clicked the pattern seed (the seed text changes and the hero field is drawn again), hovered the newest commit (it gets `lit` and its parent `parent-lit`), replayed seed `0x7334716c` twice (the two canvases are byte-identical as data URLs, and the output reads "run 2: the same field as run 1"), drew a new seed, and submitted an invalid seed (the hint about 8 hexadecimal digits appears).

### Why

A short page has room for few claims, so each one must be right. The draft's front end row overstated what the parser checks, and the scalar subquery row understated what was built.

### What worked

Checking each fact against the named file before writing it. The chip row, the rail, and the roadmap's three-column grid took the new text without any change to the CSS.

### What didn't work

The worktree's isolation refused two commands. A Python script that did all the replacements at once: "this command is too complex to verify that it stays inside the worktree". And `playwright-cli eval "…"`: "this command runs a string through eval, which can't be verified to stay inside the worktree". I used the Edit tool for the text, and `playwright-cli run-code` with scripts in the scratchpad for the measurements.

`playwright-cli -s=fp open` from the worktree wrote a snapshot to `/.playwright-cli/` in the worktree. I deleted it, and ran later browser commands from the scratchpad, where a session opened from the worktree is not found: "The browser 'fp' is not open, please run open first". Reopening from the scratchpad fixed it.

The branch name `front-page` from the brief was taken: `git checkout -b front-page` gave "fatal: a branch named 'front-page' already exists", checked out in another worktree at `c59b37b` with no commits of its own. I used `front-page-v2` and left the other alone.

My first replay check compared the canvas before any replay with the canvas after one, and they differed. The animation ends at `1 - 2^-10` of the width, so the last pixel or so of columns is left out after an animated run. That is in the script on `main` too, and not part of this change. Two animated replays of the same seed are identical.

### What I learned

At 390 pixels the state chips wrap to three rows with "SQL on one table built". I tried "One-table SQL built" and it still took three rows, since "Not ready for use" needs about 160 of the 349 pixels on its row. Only a chip without "built" would fit two rows, so I kept the clearer wording.

### What was tricky

Saying where the memory limit applies without a long sentence, and keeping the rows few. A row of its own, worded for any statement, was shorter than a clause that names `returning`.

### What warrants review

The nine roadmap rows in the `#roadmap` section, and whether "A memory limit" deserves a row or should fold into another. The `Interface` row in "What it does" wraps to two lines on a wide screen and three at 390, because the call is long. Full-page screenshots are in the lead's scratchpad as `front-page-v2-1440.png` and `front-page-v2-390.png`.

A self-review with the code-review skill, at low effort, over both steps found no bugs. It checked that the glyphs stay visible because `.invert` sets `--ink` to `--void`, and that no link to `#status` and no hook of the removed legend is left. It raised one point that is not a bug: the rule in `/AGENTS.md` on the public site still tells the lead to update the section with the id `status` in `/site/index.html`, which this change removes. The lead owns `/AGENTS.md`.

### Future work

The `meta` description and the Open Graph description still say "written and owned by AI agents, and trusted with simulation testing", which is still true, so I left them.
