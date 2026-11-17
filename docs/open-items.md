# Open items

Outstanding work on the docs and the product: what is missing, what is
stale or contradicts itself, and what is still undecided. Each item ends
with a status, `open` until it is settled.

## A. Urgent

1. **Vendor doc-portal passwords are in a public repo.**
   `PROMPT_dont_delete.md` is tracked, has been pushed to
   `github.com/webduvet/fintechlab-simple` (public) since `75d9a6d`, and
   contains the B4B and Banking Circle documentation passwords. This breaks
   the "no vendor secrets" rule in `principles.md`. To decide: delete the
   file, rewrite history or not, and tell the vendors so they can rotate. — open
2. **Parked kernel/pod material is still on `main`.**
   `docs/architecture/{banking-circle-twin,kernel-pod-review-notes,settle-plugin-b4b}.md`
   describe `core/`, `recipes/`, `:9085` pods and `make up-pods`, none of
   which exist here. `catalogue.md` § *Kernels and pods* links
   `../core/README.md`, which is a dead link. The `.gitignore` `pod-data/`
   comment refers to `core/pod`. — open

## B. Documentation that is stale or contradicts itself

3. **The service and port lists disagree with each other.** The README port
   table has no `clock` (:8096) and no `verification` (:8089).
   `structure/README.md` says "eleven binaries", but there are 14 (`clock`,
   `console` and `verification` are missing), and its aside says compiled
   binaries are committed, which `.gitignore` has since fixed.
   `deploy-pod.md` says "published: 8080-8090, 8095, 8443, 2222", which
   leaves out 8096. — open
4. **`verify` and `verification` overlap, and the docs never explain why
   both exist.** The catalogue's second table has rows for `verify` and
   neither `verification` nor `clock`. Which one does the platform call,
   and does `verify` still need to exist? — open
5. **B4B persistence is described two ways.** `console.md` § Batches says
   "B4B's beneficiary store is in memory". The catalogue and
   `ARCHITECTURE-b4b-oversight.md` say everything is persisted to
   `b4b-data/state.json`. — open
6. **The console's "five-minute demo" is out of date.** It refers to a
   *Services* view (now Vendors/Platform) and a Settlement card, but the
   stand-ins are now hidden by default. — open
7. **Four architecture docs describe designs that have been replaced.**
   `ARCHITECTURE-settlement-report`, `-sftp-staging` ("not a real SFTP
   server"), `-reconciliation-api` and `-settlement-state-machine` are based
   on Bambora North America's REST and WX model and grow the `settlement`
   stand-in, which the catalogue says "does not grow". The catalogue still
   lists them as current. They should be marked superseded or removed. — open
8. **The phase-named docs are change logs, not specs.**
   `ARCHITECTURE-vendor-corrections.md` (595 lines) and
   `-phase3-corrections.md` (469 lines) record *what was fixed*. No single
   page per vendor states the contract as it is today: endpoints, auth,
   wire shapes, lab-only hooks, known deviations. You currently have to
   read the catalogue row, the architecture doc, the corrections addenda,
   `structure/<vendor>.md` and the console doc to put one together. — open
9. **`services/fake-bank.md` predates the business-bank role.** It doesn't
   cover `POST /internal/credit`, accounts opened on first sight, or the
   *Payouts credited* log. — open
10. **Worldline merchant enrolment** (boarding receipt on `to_WLNORDIC`,
    `cmd/worldline/enrolment.go`) is missing from the catalogue row,
    `console.md` and the runbooks. — open
11. **Platform-specific material lives in a platform-agnostic lab.** The
    runbooks and getting-started are written for buddy: buddy paths, buddy
    branch names (`feature/fintechlab-file-actions`), `settle_db` SQL, and
    `/home/andrej/...` in the Playwright snippet. That's fine as a reference
    walkthrough, but where should platform-specific docs live: here, in
    buddy, or in `fintechlab-runner`? — open
12. **The console's Docs view versus `docs/`.** What does it show, and how
    is it kept in sync? — open

## C. Unclear or still to be decided (product)

13. **The real platform's run skips the Worldline SFTP+PGP path.** The
    flow step says *"a run triggered from S3 does not use it … collected by
    acquirer-fts, not by a run started here"*. The runner generates its own
    files and funds the SGA itself (`/sim/fund-sga`). As a result the lab's
    most protocol-accurate piece, and the Worldline lump sum, are only
    exercised by the stand-ins and the harness, never by the platform loop
    the lab exists for. — open
14. **Two merchant worlds.** The console registry boards merchants into B4B
    and Worldline, while the runner seeds merchants into buddy's DBs and
    writes its own files. Which one is the source of truth, and how do the
    two meet (`GET /api/merchants` → platform seed is described but not
    exercised)? — open
15. **Who funds the safeguarding account in a platform run?** Is it the
    Worldline lump sum or the runner's `fund-sga`? And should the platform
    receive BC's `IncomingPayment*` webhook for it? (Phase 3 flagged this
    as step [3].) — open
16. **Persistence policy.** Banking Circle and the clock keep everything in
    memory, while B4B and the console registry are on disk. Losing BC state
    on restart causes most of the "looks broken but isn't" list (dead
    subscriptions, 404 recon reads, "restart the runner"). Decide per vendor
    what survives a restart. — open
17. **Failure injection is called a "later concern", but much of it already
    exists**: BC reject/hang/return/reverse/pause, verification outcomes,
    B4B sanctions/disabled/force-fail. Is there a model for it (a scenario
    catalogue, named presets, a harness assertion per failure), or is it a
    growing set of one-off hooks? — open
18. **ACI's place in the flow.** A card payment at ACI doesn't become a
    Worldline transaction in the lab. Should an ACI payment feed the
    acquirer's transactions, so that one customer payment can be traced
    through to the payout? — open
19. **The Worldline REST API.** The original brief asked to "simulate the
    worldline api we interact with". Today it's a "documented seam". Does
    buddy call any Worldline REST endpoint, and if so, which? — open
20. **Chargebacks and refunds.** A file cut by the lab always has an empty
    `CB` section, yet the runner's generated files have chargebacks,
    refunds and reversals. The lab-cut file can't express what the
    platform already tests. — open
21. **Calendars.** BC has "no bank holidays", and the clock has
    GB/Paris/Stockholm calendars. Are holidays needed for cut-off testing? — open
22. **A second acquirer.** "In the future we might have more." Is the lab
    shaped so a second acquirer is a new package, or does Worldline leak
    into shared code (the console flow, the settlement plugin descriptor's
    "settles Worldline files")? — open
23. **Report emails.** "We report and distribute emails." With no mailer,
    every run is amber. Should the lab add a mail-sink vendor (SMTP capture
    plus a card)? — open
24. **Places where green here doesn't mean green in production.** BC
    accepts any Basic auth, and B4B auto-vivifies unknown beneficiaries.
    Tighten them, or keep them and list them? — open
25. **Open B4B vendor questions** (`ARCHITECTURE-b4b-oversight.md` § *what
    to confirm*: callback body, `nace_codes`, `registered_company_no`,
    same-company beneficiaries, unknown fields). Who asks the vendor, and
    where is the answer recorded? — open
26. **mTLS default.** "We don't use mTLS", but getting-started depends on
    the lab's mTLS client cert for BC. Which is the default, and which is
    the option? — open
27. **Audience and product.** Is this an internal InfinitePay test harness,
    the public "fintech sim lab" (tutorials and recipes), or both? The answer
    sets the tone of the docs, how much buddy belongs here, the licence, and
    when the parked kernel/pod work comes back. — open
28. **A shared or remote lab.** `deploy-pod.md` offers a server deployment,
    but the console has no auth and anyone can register a plugin. Is
    multi-user sharing a goal? — open
29. **Acceptance criterion for "a happy path that actually connects".**
    The harness proves the vendors using stand-ins. What proves the
    *platform* loop: a scripted end-to-end check against a registered
    plugin, or the manual getting-started walkthrough? — open
30. **Keeping up with vendor API changes.** When BC, B4B or Worldline
    change their APIs, how does the lab find out, and what pins the
    contract (OpenAPI snapshots, fixtures, the buddy client's types)? — open
31. **The "product family" in `design-system.md`.** It's written "so
    another operator app in this product family comes out looking the
    same". Which apps? — open
