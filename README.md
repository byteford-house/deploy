# homelab-deploy

Go rewrite of `deploy.sh`. Same commands, same `.deploy-deps` /
`.manual-check` / `.manual-before` conventions, same `services/`, `config/`,
`infra/` layout — drop it in next to the existing directories and it works
the same, with one deliberate change: the shared config file is now `.env`
(see below) instead of `env.sh`.

## Flags

`homelab-deploy [flags] <command> [command args]` — flags may appear
anywhere on the command line (`pflag`'s interspersed parsing), so
`homelab-deploy plan --verbose` and `homelab-deploy --verbose plan` both
work.

| Flag | Shorthand | Default | Does |
|---|---|---|---|
| `--working-dir <path>` | `-C` | current directory | Root directory to operate in (`services/`, `config/`, `infra/` live under it — or, in multi-repo mode, wherever `.deploy-repos` is) — matches the `git -C`/`make -C` convention. Implemented as a `Chdir` once at startup, so every existing repoRoot-relative assumption in the codebase (the report path `open` looks for, the `<dir>` existence check, etc.) just keeps working correctly rather than needing an override threaded through separately. |
| `--env-file <path>` | | `<working-dir>/.env` | Load a specific secrets file instead of the default — e.g. switching between `.env.staging` and `.env.prod` without renaming files. |
| `--shared-env-file <path>` | | `<working-dir>/.env.shared` | Same idea, for the committed non-secret file — see "Two env files" below. |
| `--verbose` | `-v` | off | Stream live terraform output to the terminal during `plan`/`apply`, prefixed by directory (`[services/a] ...`), instead of only the one-line summary per directory. Reuses the exact same `Hooks.LiveWriter` mechanism the web server's SSE streaming already uses — no separate code path. Concurrent directories (plan) share one mutex so output can't interleave mid-line. |
| `--timeout <duration>` | | `0` (none) | Maximum time a `plan`/`apply`/`init` run may take, e.g. `10m`. Without this, a hung provider call blocks forever with no way out except Ctrl+C. Not used by `serve`, which runs indefinitely by design. |
| `--detailed-exitcode` | | off | `plan` only: exit `0` when no directory has changes, `2` when at least one does, `1` on any error. See "Exit codes and CI output" below. |
| `--summary-md <file>` | | none | Write a short Markdown summary of the run (for a PR comment or job summary) to `<file>`. Works with `plan`, `apply` and `<dir> plan`; attribute values and raw terraform output are left out on purpose. See "Exit codes and CI output" below. |
| `--migrate-backend` | | off | With `init`: migrate to the `[backend]` configured in the env files, after an explicit confirmation. See "Remote backend" below. |
| `--insecure-no-auth` | | off | `serve` only: run the dashboard with **no authentication**. Without it, `serve` refuses to start unless the `HOMELAB_DEPLOY_OIDC_*` settings are present — see "Authentication and roles" below. |

Verified `--working-dir` for real by running the binary from `/tmp` against
a repo at `/tmp/flagrepo` — `init`/`plan` operated on the right directory,
and critically the HTML report and `open` command both resolved to
`/tmp/flagrepo/.deploy-logs/`, not a path relative to the actual shell
`cwd`, confirming the `Chdir` approach actually fixes the class of bug
it's meant to (a naively-threaded `repoRoot` parameter would have missed
at least the `open` command, which builds its path independently).
`--env-file`, `--verbose`, and `--timeout` were each verified the same
way — a custom env file loading with no "not found" warning, live
prefixed output actually appearing before the summary line, and a
deliberately tiny timeout genuinely producing a `context deadline
exceeded` error rather than silently being ignored.

### Exit codes and CI output

Exit status of `plan` (every directory, or `<dir> plan`):

| Exit | Meaning |
|---|---|
| `0` | every directory planned and none has changes (or there are changes but `--detailed-exitcode` was not given) |
| `1` | at least one directory failed to plan, or the run itself failed (bad flag, no terraform, ...) |
| `2` | `--detailed-exitcode` only: every directory planned, and at least one has changes |

Errors win over changes, so `2` always means "a clean plan with pending
changes". **Before this flag existed, a `plan` in which some directory
failed still exited `0`; it now exits `1`, with or without
`--detailed-exitcode`.** A scheduled drift check is then just
`homelab-deploy --non-interactive --detailed-exitcode plan` and treating
`2` as "drift" and anything else non-zero as "broken".

`apply` exits `0` on success and `1` if any directory failed (it stops at
the first failure); `--detailed-exitcode` has no effect on it.

`--summary-md <file>` writes a short Markdown digest: one line per
directory (errors first, then changes, then a single line listing the clean
ones) with the add/change/destroy counts, each changed resource's address
and action, warning headlines, and only the **first line** of an error. It
never contains attribute values, raw terraform output, warning detail or
the rest of an error message, since any of those can carry a secret; the
full detail stays in `.deploy-logs/` (upload it as an artifact). Long
lists are truncated so the file stays under GitHub's 65536-character
comment limit. The file is written even when the run fails, so a PR
comment can say why. It is rejected (rather than silently ignored) with
commands that have nothing to summarise.

#### `.deploy-logs/report.json`

Written by `plan` (always) and by an `apply` that fails; a successful
`apply` does not write it. There are no JSON struct tags, so the keys are
the Go field names, which a test pins (`internal/report`). Shape:

```json
{
  "GeneratedAt": "2026-11-05T12:00:00Z",
  "Command": "plan",
  "Entries": [
    {
      "Dir": "services/web",
      "Status": "CHANGES",
      "Detail": "1 to add, 1 to change, 0 to destroy",
      "ResourceChanges": [
        {
          "Address": "nomad_job.web",
          "Action": "update",
          "Attributes": [{ "Path": "env.TOKEN", "Before": "(sensitive value)", "After": "(sensitive value)" }]
        }
      ],
      "Adds": 1, "Changes": 1, "Destroys": 0,
      "RawOutput": "…terraform's human-readable output…",
      "UnsavedPlan": false,
      "RemotePlanURL": "",
      "Warnings": [{ "summary": "Argument is deprecated", "detail": "…" }]
    }
  ],
  "Graph": [{ "Dir": "services/web", "DependsOn": [], "Depth": 0, "ManualStep": false, "NotSynced": false }]
}
```

- `Status` is `OK` (no changes for `plan`, applied cleanly for `apply`),
  `CHANGES` or `ERROR`; for `ERROR`, `Detail` is the error text.
- `Action` is `create`, `update`, `delete` or `replace`; `Attributes` is
  only filled for `update`/`replace`. Sensitive values are masked as
  `(sensitive value)`.
- `Warnings` keys are lowercase, everything else is capitalised (an
  inconsistency that predates this documentation and is kept for
  compatibility).
- `RawOutput`, `Detail` of an error and `Warnings[].detail` are terraform's
  own text: treat the file as sensitive and do not paste it into a comment.
  Use `--summary-md` for that.
- `Graph` can be `null` (e.g. `<dir> plan`).

Ideas for further flags, not built since they'd either duplicate an
existing mechanism or need more design than a single flag warrants:
`--concurrency` (redundant with `DEPLOY_PLAN_PARALLELISM`, which already
covers this), `--json` (machine-readable stdout for scripting — the
per-run `report.json`, documented above, already covers the common case), `--only`/`--skip`
<dir> filters for a full run (mentioned under "Extending it later" below,
since it interacts with dependency-graph semantics enough to deserve its
own thought rather than a quick flag).

**A directory named exactly `plan`, `apply`, `init`, `sync`, `graph`,
`open`, or `serve` can never be targeted as `homelab-deploy <dir>`** —
`run`'s dispatch checks known subcommand names first, so a same-named
directory is always interpreted as the subcommand instead. Rename the
directory, or run terraform in it directly, if this ever comes up in
practice; none of the conventional `services/`, `config/`, `infra/`
layouts collide with a subcommand name, so this is intentionally left
undocumented-in-code rather than "fixed" with an ambiguity-prone
disambiguation rule.

## Two env files

`.env` (secrets, gitignored) and `.env.shared` (committed, non-secret)
— same `[tfvars]`/`[env]` format as before, loaded in that order
(`.env.shared` first, so `.env` is a local override of the shared
default; `[backend]` settings merge the same way, key by key). A bucket
name or region isn't a secret; the credentials to access that bucket
are — this split lets the former live in git and the latter not,
without a third config file or a schema to maintain by hand. See
`.env.example` and `.env.shared.example`.

## Multi-repo mode

If you have several git repos that need deploying in a specific order —
what used to require a script to clone them all and remember that
order — put a `.deploy-repos` file at your `--working-dir` root and
each line becomes a node in the exact same dependency graph everything
else in this tool already uses. No other package needed to learn a
second way of doing things: `graph`, `tf`, `orchestrate`, `report`, and
`server` all still just work with a plain directory-name string as a
node's identity, completely unaware that anything's different. The one
seam is `internal/manifest.Resolver` — the single place that identity
turns into a real filesystem path, either `filepath.Join(repoRoot,
name)` (single-repo mode, whenever no `.deploy-repos` exists — nothing
changes if you don't use this) or a manifest entry's own location.

**Format** — one entry per line, `name=url[#ref] [tf-root-subpath]` or
`name=path [tf-root-subpath]`:

```
services-auth=git@github.com:acme/services-auth.git
services-web=https://github.com/acme/services-web.git#main
infra-host=git@github.com:acme/infra-host.git#v3 deploy/tf
legacy-service=/opt/manual-checkouts/legacy-service
```

A value with a scheme (`https://`, `ssh://`, `git://`, `file://`) or
SCP-style `user@host:path` is git-managed — `sync` clones and updates
it. Anything else is a local path (absolute, or relative to
`--working-dir`) — used exactly as-is, never touched by `sync`, no
different from a plain directory today. The optional trailing subpath
is where a repo's actual Terraform root lives, if it's not at the
repo's own top level; `#ref` only makes sense for a git entry, and is a
hard error on a local-path line (catches the copy-paste mistake of
forgetting to swap in a real URL).

**A leading `/` means an absolute filesystem path, not "relative to
`--working-dir`".** `services/vault=/services/vault` doesn't mean "the
`services/vault` folder in my repo" — it means the literal path
`/services/vault` from your filesystem's root, which almost certainly
doesn't exist. Write `services/vault=services/vault` (no leading `/`)
for a path relative to wherever `.deploy-repos` lives. This exact
mistake was the first real bug this feature hit in practice: every
entry silently showed zero dependencies, with no error at all, because
a missing `.deploy-deps` file is (correctly) treated as "this directory
has none" — which looks identical to "this directory doesn't exist"
unless something specifically checks for the difference. Now something
does: a local (non-git) entry whose path doesn't exist fails `graph`
immediately with a clear message naming the entry and hinting at this
exact gotcha, rather than staying silent — deliberately scoped to local
entries only, since a *git* entry's path legitimately doesn't exist
until `sync` has run, which isn't a mistake.

**A not-yet-synced git entry gets a clear message too, everywhere.**
Running `plan`/`apply`/`init` against a git entry before ever running
`sync` used to surface a raw `stat: no such file or directory` from
deep inside `terraform-exec` — technically accurate, but not something
that obviously points at "you forgot to sync." It now says exactly
that: `"vault" hasn't been synced yet — run "homelab-deploy sync"
first`. `graph`'s text and dot output mark a not-yet-synced entry too
("not synced — run `homelab-deploy sync`" in text, a dashed
light-coral box in dot) rather than showing it identically to a
directory that genuinely has no dependencies — the same category of
"silent vs. loud" gap as the leading-slash bug above, just in the
other direction (missing entirely vs. present but not yet fetched).

**The web dashboard has a "Sync All" button too** — shown only when
`.deploy-repos` actually exists (nothing to sync otherwise), driving
the exact same job/live-pane/SSE machinery Plan/Apply/Init already use.
Each repo gets its own live pane with real git output as it happens,
same as a directory's own pane during `plan`/`apply`. `manifest.Sync`'s
signature is a per-entry `func(name string) io.Writer` callback rather
than one shared writer specifically so this could reuse the existing
per-directory live-pane pattern instead of inventing a second one; the
CLI's own `sync` output got the same per-repo-prefixed treatment as
`--verbose` plan/apply, for free, from the same change.

**`homelab-deploy sync`** clones anything not yet checked out and
fast-forward-updates anything that is. It's deliberately conservative:
a dirty working tree, or a local commit that can't fast-forward onto
the target ref, both come back as an error for that specific entry
rather than ever being discarded or force-overwritten — resolving that
is left to you, on purpose. Every entry is still attempted even if an
earlier one fails, since one repo's problem shouldn't block updating
everything else (unlike `apply`, sync has no dependency order to
respect — the dependency graph only matters once terraform actually
runs). Local-path entries are always skipped, untouched.

**Every git-managed entry clones into one `.deploy-repos-checkouts/`
subfolder**, not scattered across the top level of `--working-dir` —
`.deploy-repos-checkouts/vault/`, `.deploy-repos-checkouts/services/authentik/`,
and so on. One directory means one `.gitignore` line
(`.deploy-repos-checkouts/`) covers every clone, rather than a
per-repo entry or your orchestration repo's `git status` filling up
with untracked directories after the first sync. This only applies to
entries `sync` actually clones — a local-path entry already gives you
full control over where it lives (that's the point of it), so it's
completely unaffected and keeps using exactly the path you gave it.
Only the entry's *name* is used anywhere else (the dashboard, plan
file names, backend state keys via `{dir}`) — this only changes where
the actual checkout sits on disk.

**Upgrading from an older version of this tool**: git-managed entries
used to clone directly at the top level (`<name>/`, no subfolder). If
you already have checkouts there, `sync` won't find them at the new
location and will happily clone fresh copies into
`.deploy-repos-checkouts/` instead — harmless, but it means you'd end
up with two copies and, if you're on local Terraform state, a
from-scratch checkout with no state history. Move your existing
checkouts into the new location once (`mkdir -p
.deploy-repos-checkouts && mv <name> .deploy-repos-checkouts/<name>`
for each git-managed entry) before running `sync` again, and
everything — including any local state already inside them — carries
over exactly as it was.

**Authentication is entirely delegated to your system's own git/SSH
setup** — `sync` shells out to the real `git` binary, with no custom
credential handling of its own. If `git clone <url>` already works when
you type it yourself in the environment `homelab-deploy` runs in, `sync`
will work identically; if it doesn't, `sync` won't either.

- **SSH URLs** (`git@github.com:...`) use whatever SSH agent or key
  files are already set up for the user/process running
  `homelab-deploy` — nothing repo-specific to configure by default. To
  point at a specific deploy key instead of relying on an agent or a
  default `~/.ssh/id_*`, set it once via the `[env]` section of
  `.env`/`.env.shared` (which already sets arbitrary environment
  variables for the whole process, inherited by every subprocess
  including git):
  ```
  [env]
  GIT_SSH_COMMAND=ssh -i /home/user/.ssh/deploy_key -o IdentitiesOnly=yes
  ```
- **HTTPS URLs** use whatever credential helper, `.netrc`, or cached
  credentials git already has configured. Don't embed a token directly
  in the URL inside `.deploy-repos` (`https://TOKEN@github.com/...`) —
  unlike `.env`, `.deploy-repos` isn't a secrets file and is generally
  fine to commit (repo names and URLs aren't secret by themselves), so
  a token embedded there would leak into whatever gets committed.
- **Never hangs waiting for a password that will never come** — `sync`
  can run unattended (the dashboard's Sync button, a cron job), so a
  private repo with no credentials configured needs to fail with a
  clear error, not hang forever. Verified directly: a real `git clone`
  against an inaccessible HTTPS URL failed in ~6 seconds with a clear
  git error rather than hanging. Every git subprocess also explicitly
  gets `GIT_TERMINAL_PROMPT=0` (on top of the inherited environment —
  a custom `GIT_SSH_COMMAND` you've set is never clobbered by this), so
  the guarantee is intentional rather than an incidental side effect of
  how the subprocess's stdin happens to be set up.

A genuine bug came out of testing this for real against actual git
repos, not just unit tests, worth being upfront about: running
`terraform` inside a cloned repo leaves `terraform.tfstate`/`.terraform/`
behind as *untracked* files, which made the very next `sync` — after
the completely ordinary clone-then-apply workflow — see the tree as
dirty and refuse to touch it. Fixed by having `sync` add Terraform's own
artifacts to the repo's **local** git excludes (`.git/info/exclude`, not
its tracked `.gitignore`, which isn't ours to modify) the first time it
touches a repo — idempotent, and applied before the dirty-check so a
repo already stuck in this state from before the fix un-sticks itself
on the next sync. A real edit to an actually-tracked file sitting right
next to those artifacts still correctly blocks sync — confirmed with a
test specifically checking the exclude list doesn't become a blanket
bypass.

**Verified for real**, the same way as everything else in this tool:
two real bare git repos (one with its Terraform root deliberately
nested in a `terraform/` subdirectory, to prove the tf-root subpath
actually works, not just parses), a real `.deploy-deps` dependency
between them, and a local-path entry mixed in — `sync`, `graph`
(showing no dependency before syncing, since `.deploy-deps` lives
*inside* the checkout and genuinely can't be read yet — and the correct
`[depends on: ...]` annotation once it's synced), `init`, and `apply`
all run end to end, confirming: apply happens in the right order,
state lands inside the nested `terraform/` subdirectory specifically
(not the repo root), the local-path entry is never cloned and its state
lands exactly where it already was, and the HTML report/dashboard both
render correctly across all three entries together.

## Remote backend

`homelab-deploy init --migrate-backend` migrates a directory (or every
not-yet-migrated directory at once) from local state to whatever
`[backend]` your `.env.shared` (or `.env`) configures. The same `[backend]`
settings are passed as `-backend-config` whenever `init` runs for a
directory that has `deploy_backend.tf`, including from the `serve` dashboard.

**The hard constraint this works within.** Terraform requires a
backend's *type* to be declared as a literal block physically inside
each root module's own `.tf` files — there's no way to supply that
purely via CLI flags or environment variables. Everything *inside* the
block (bucket, region, credentials, state location) genuinely can come
from outside via `-backend-config`, which is what `[backend]` becomes at
init time — but the type declaration itself is unavoidable. Rather than
asking you to hand-write that stub in every directory, `--migrate-backend`
generates it for you: `deploy_backend.tf`, a small, clearly-marked,
**meant-to-be-committed** file. Its presence is what "already migrated"
means (idempotency and the batch mode below are both based on checking
for it), and its git history is the answer to "when did this switch
backends, and to what" — a question that's otherwise easy to lose track
of once a project has been through a few infrastructure changes.

**The `{dir}` placeholder.** Backend types don't agree on what the
"where does this directory's state live" setting is even called —
s3/azurerm call it `key`, the `local` and `consul` backends call it
`path`, gcs calls it `prefix`, postgres calls it `schema_name`.
Hardcoding one name would be silently wrong for most backend types (or
worse: silently collide every directory onto the same state). Instead,
put the literal placeholder `{dir}` in whichever setting is actually
correct for your backend — `key={dir}.tfstate` for s3, `path=/state/{dir}.tfstate`
for local/consul — and `homelab-deploy` substitutes it with each
directory's own path before passing it to terraform. `--migrate-backend`
refuses to run at all if no setting contains `{dir}`, rather than
quietly proceeding into a collision.

**The confirmation is real, not decorative.** `terraform-exec` always
passes `-input=false` to `terraform init` — that's hardcoded on its
side, not something any option can turn off — so Terraform's own
interactive "do you want to copy state to the new backend?" prompt can
*never* actually appear when this tool is driving it. Whatever
confirmation exists has to come from us, so `--migrate-backend` prints
the backend type and every affected directory's **fully resolved**
settings (the actual value with `{dir}` substituted, not the template)
and requires typing the literal word `yes` — not Enter, not `y` — before
touching anything. There's deliberately no `--force`/`--yes` flag to
skip this; if a real scripted/CI need for one shows up later that's an
easy, separate addition, but it shouldn't exist before there's an actual
need for it.

**Batch mode is one confirmation, not a script.** `homelab-deploy init
--migrate-backend` (no directory argument) finds every directory that
doesn't already have `deploy_backend.tf`, shows all of them in one
preview, and migrates them one at a time — sequential, stopping at the
first failure, so a partial failure leaves a clearly-defined "these are
done, this one failed, the rest weren't touched" state rather than
several failing at once. Already-migrated directories are silently
skipped, which is what makes re-running it later (after adding new
directories) safe and useful rather than something you have to
remember to scope manually. `homelab-deploy <dir> init --migrate-backend`
does the same thing for just one directory — good for trying the
process on something low-stakes first.

**Correct handling of a failed migration.** The backend stub has to
exist *before* `terraform init` can even attempt the migration — but if
that migration then fails, leaving the stub in place would make a retry
think the directory was already handled. `migrateOneDir` in `main.go`
rolls the stub back on failure, so `HasBackendStub`-based idempotency
stays correct: a failed directory is still "not yet migrated" on the
next run.

**Why `-force-copy`, not `-reconfigure`.** These are Terraform's two
different responses to a changed backend configuration: `-reconfigure`
explicitly *discards* the old state reference and starts fresh (the
opposite of what a migration should do), while `-force-copy` actually
copies existing state into the new backend non-interactively. Per
Terraform's own project history ([hashicorp/terraform#28718](https://github.com/hashicorp/terraform/issues/28718)),
`-force-copy` implies `-migrate-state`'s behavior — and `terraform-exec`
exposes `-force-copy` (`tfexec.ForceCopy`) but not `-migrate-state`
directly, so this is both the semantically correct choice and the only
one actually available through the library.

**Verified for real**, not just unit-tested: applied a real
`terraform_data` resource under the default local backend, migrated it
to an *explicit* local backend at a different path (the exact same
`-force-copy`/`-backend-config` codepath a real S3/Consul/etc. migration
uses, fully offline — no cloud credentials or network needed to prove
the mechanism), and confirmed via both the new state file's actual
content and a post-migration `plan` reporting zero changes that the
state was genuinely copied over, not just started fresh empty. Also
drove the full CLI end-to-end: declined a confirmation (nothing
happened, no stub written), confirmed one (state correctly appeared at
the new location), ran the batch version across two directories (one
already migrated, correctly skipped; the other correctly picked up),
re-ran the batch with nothing left to do (correctly reported so), and
confirmed the missing-`{dir}` validation rejects a bad config before
ever showing a confirmation prompt.

Writing the real integration test for this caught two genuine bugs
before they shipped, worth being upfront about:
1. **A design mistake**: my first pass hardcoded `-backend-config="key=..."`
   for every backend type, which is wrong for anything that isn't
   s3/azurerm-shaped (see the `{dir}` placeholder explanation above —
   that design replaced this one).
2. **A real, previously-latent data race**: `Plan`/`Apply`/`MigrateBackend`
   all share one `*bytes.Buffer` between a terraform command's stdout and
   stderr, which `terraform-exec` reads on separate goroutines —
   `bytes.Buffer` isn't safe for concurrent use, and this had apparently
   been present since `Plan`/`Apply` were first written, just never
   triggered by `-race` before because no earlier test produced
   genuinely concurrent stdout+stderr output. A real terraform error
   (an invalid backend type, in the failed-migration test) was the first
   thing to actually hit it. Fixed with a mutex-wrapped writer
   (`syncWriter` in `tf.go`) that both this feature and the existing
   `Plan`/`Apply` now go through.

## Backends that can't save a plan file

Terraform's `remote` backend (Terraform Cloud/Enterprise, when a
workspace uses *remote execution* — not just remote state) refuses to
save a generated plan locally at all: `Error: Saving a generated plan
is currently not supported`. Terraform Cloud itself manages the plan
artifact in that mode; there's no local file to hand back. This
directly conflicts with how this tool has always worked — `plan` saves
a file, `apply` later applies *exactly* that file — for any directory
using a backend like this.

**`plan` for a directory like this just shows what would change,
without trying to save anything.** If saving fails with that specific
error, it retries as a plain `terraform plan` (no `-out`) — through the
exact same streaming path every other directory already uses, not a
separate implementation, so the live pane/dashboard shows the same real
diff it always would. The summary counts come from terraform's own
human-readable `Plan: 3 to add, 1 to change, 0 to destroy.` line — the
same universal text the original bash version of this whole tool
grepped for everywhere, and specifically the thing Terraform Cloud's
remote execution is designed to relay faithfully, since matching the
local experience is the whole point of that feature. The one thing not
available is the structured per-resource list every other directory's
report gets (that needs JSON from a saved plan file, and there isn't
one here) — the full diff is still right there in the plain text output,
just not also broken into that table.

**A link to the run in Terraform Cloud's own UI shows up wherever this
directory's result does.** Remote-execution output includes its own
"To view this run in a browser, visit: &lt;url&gt;" line — `Plan` extracts it
(`parseRemotePlanURL` in `tf.go`) and carries it alongside the rest of
the result (`PlanResult.RemotePlanURL` → `report.Entry.RemotePlanURL`)
into the static report, the dashboard's last-result table, and the live
job page (as an SSE field on the same `status` event that carries
status/detail) — one place this gets set, three places it's shown,
rather than three separate parsers. Since TFC has the structured detail
this backend can't hand back locally, the link is the fastest way to
actually see it. Matched by text, same caveat as `isSavedPlanUnsupported`
itself: this is Terraform's own wording, not something typed, and (per
"What's verified here, and what isn't" above) hasn't been confirmed
against a live Terraform Cloud run — if the exact phrasing ever differs,
`RemotePlanURL` is simply empty and nothing links out, same as today.

**`apply` re-plans and requires an explicit "yes" before applying.**
There's no saved plan to silently trust here — there never was one to
review — so the reviewing has to happen immediately before applying,
not earlier. Concretely: `apply` first tries to save a plan fresh (this
works transparently, with no confirmation needed, for the ordinary case
of a directory that simply hasn't been planned yet — any backend that
*can* save one just gets the same guarantee as always, computed
just-in-time instead of earlier). Only if that fresh save itself fails
with the same "can't save" error does it re-plan once more, plain this
time, print the exact diff, and require the literal word `"yes"` before
applying it — same confirmation style as `--migrate-backend`'s prompt,
for consistency.

**This confirmation is CLI-only, on purpose.** A browser has no stdin
to type "yes" into. The web dashboard's Apply button uses
`orchestrate.DefaultUnsavedPlanHandler`, which always skips a directory
in this state — same safety posture this tool already uses for manual
steps (`.manual-before`) it can't interactively pause for either — and
reports a clear error telling you to run `homelab-deploy <dir> apply`
from the CLI instead, rather than ever silently auto-approving
something nobody actually reviewed.

**This replaced an earlier, more complicated design**, worth being
upfront about since it went through several rounds of real-world
debugging before landing here. The first version tried to recover the
*same* rich, structured per-resource detail every other directory gets,
via `terraform plan -json`'s machine-readable event stream. In
principle that's a strictly better result when it works. In practice,
verified against a real Terraform Cloud remote-execution workspace, it
didn't: the run completed but reported "0 to add, 0 to change, 0 to
destroy" for a directory that demonstrably had real changes, and a
follow-up fix surfaced a second, more fundamental bug — the retry's own
output never reached the live pane at all, making a normally-behaving
retry indistinguishable from a hang. Both were real, fixable bugs, but
they kept recurring because the underlying approach depended on
guessing the shape of a JSON stream that can't be reproduced or
verified outside a real Terraform Cloud remote-execution workspace,
which isn't available in the environment this was built in. Dropping
down to plain text output sidesteps that entire category: it only ever
relies on the one thing actually confirmed to work reliably — the
human-readable summary line, which Terraform Cloud is specifically
designed to preserve — and it means the live-pane bug is now
structurally impossible here rather than merely fixed, since there's no
longer a separate code path for this case to accidentally diverge from
the normal one.

**What's verified here, and what isn't, honestly**: reproducing the
actual "remote" backend's refusal-to-save error still isn't possible in
this environment, so the exact trigger condition remains unconfirmed
end-to-end by a live run against real Terraform Cloud infrastructure.
What I did verify for real: applying a directory that was never
pre-planned, through both the CLI and the web dashboard, correctly
plans, saves, and applies transparently with no confirmation needed (the
new, ordinary-case behavior every backend now gets); the CLI
confirmation prompt itself, including that only the literal word "yes"
confirms; the hook-wiring that makes the web server's default safe
(skip) rather than needing a live trigger to prove out; and the full
existing test suite (210 tests) staying green throughout the rewrite,
including a real bug this refactor itself surfaced and fixed along the
way — `Apply`'s new "try to save a plan fresh" path was missing the
same directory-creation step `Plan` always does first, caught by an
existing dashboard test failing for a completely mundane reason
(`.deploy-logs` not existing yet) rather than anything related to
remote backends at all.

## Build





```
go build -o homelab-deploy .
```

I built and tested this in a sandboxed environment without access to
`proxy.golang.org` or `golang.org`, so `go.mod` currently has a
`replace golang.org/x/text => github.com/golang/text v0.14.0` line to route
around that. On your machine, with normal network access, you can almost
certainly delete that `replace` line and run `go mod tidy` to get the
canonical module — I just couldn't verify that path from here. Everything
else (`terraform-exec`, `terraform-json`, and their real dependencies) was
fetched and built for real.

## What's different from the bash version

- **`plan` output is structured, not grepped.** Each directory's plan is
  parsed via `terraform-json` into a real list of `(resource, action)`
  pairs (create/update/delete/replace) plus counts — instead of
  `grep -E '^Plan:'` against human-readable text. This is the main reason
  for the rewrite: it's robust to Terraform changing its output wording,
  and it's why the HTML report can list *which* resources are changing
  under each directory, not just a one-line count.
- **Updates show an actual attribute-level diff**, not just "update:
  nomadpack_deployment.palworld" — e.g.
  `vars.app_exporter_enabled: "false" → "true"`, extracted from
  `terraform-json`'s before/after state (`internal/tf/diff.go`) instead
  of the raw wall of CLI text (refresh lines, provider warnings,
  unrelated instructions) that used to be the only way to see what
  changed. Sensitive values are masked as `(sensitive value)` — checked
  from either side (`BeforeSensitive`/`AfterSensitive`), and propagated
  down through nested maps/lists when a whole subtree is marked
  sensitive, not just an exact-key match — the same way Terraform's own
  CLI output masks them, since this can include Vault/Authentik tokens.
  Fields with no prior value that are simply always recomputed (like a
  provider-internal `diff_hash`) are correctly excluded as noise rather
  than shown as "→ (known after apply)" on every single plan regardless
  of what you actually changed. The raw output is still there — the
  "full output" toggle is unchanged, this is additive.
- **`.deploy-logs/report.json`** is written alongside `report.html` on
  every `plan` run — the extensibility hook. Anything you want to bolt on
  later (a Slack/webhook notifier, a CI gate on `"destroys": 0`, a
  dashboard) reads this instead of scraping HTML or log files.
- **HTML report uses `html/template`** (auto-escaping) instead of the
  hand-rolled `html_escape` shell function.
- **HTML report has a second tab: "Dependency Graph".** Rendered as
  inline SVG, computed and positioned entirely server-side in Go — no
  client-side JS graphing library, no CDN, so the report stays a single
  static file that works offline. Directories are laid out left-to-right
  by dependency depth (so the picture reads in apply order) and colored
  by that run's status for the same directory: green/yellow/red/gray for
  no-changes/changes/error/not-in-this-run, with a double border on
  anything needing a manual step first. Hovering a node highlights every
  edge and neighboring node it's connected to (both what it depends on
  and what depends on it) — done with the CSS `:has()` selector, still no
  JavaScript. Tab switching is likewise pure CSS (hidden radio inputs +
  the `~` sibling selector).
- **`plan` runs every directory concurrently**, bounded by
  `DEPLOY_PLAN_PARALLELISM` (default 6 — set it in `.env`'s `[env]`
  section, or any normal way of setting an environment variable). `plan`
  is read-only, so directories don't need to wait on each other the way
  `apply` does; `apply` stays fully sequential, walking the dependency
  graph one directory at a time, since it actually mutates real
  infrastructure and downstream directories can depend on upstream ones
  having actually been applied.
- **Saved plan files live in `.deploy-logs/`, never inside a service
  directory.** `plan` writes `<slug>.tfplan` there (`services/foo` →
  `services_foo.tfplan`) so a later `apply` applies exactly what you
  reviewed, instead of re-planning and risking drift between the two. It
  deliberately does **not** live inside the actual Terraform working
  directory: it's a binary file that can contain plan-time secret values,
  and it has no business sitting next to tracked `.tf` files where it
  could show up in `git status` or get committed by accident. It's
  deleted once consumed by a successful `apply` (or immediately, if a
  plan turns up no changes) — a full `plan` run also wipes `.deploy-logs/`
  first to start clean, but a full `apply` run deliberately does **not**,
  since it needs to find the plan files a prior `plan` run just wrote.
- **Config file is `.env`, not `env.sh`, and it's no longer a shell
  script.** It's read only by this binary, so there's no reason to carry
  bash syntax or fork a subprocess to parse it. It has two sections:
  `[tfvars]` (the default — bare variable names, auto-prefixed with
  `TF_VAR_`) and `[env]` (literal environment variables, e.g.
  credentials). See `.env.example`. Values can optionally be quoted
  (`"..."` or `'...'`, stripped on read) — same convention most .env-style
  tools use, so values carried over from the old `env.sh` don't silently
  end up with literal quote characters baked in. `[tfvars]` still becomes
  environment variables rather than `-var` flags — Terraform silently
  ignores `TF_VAR_*` a root module doesn't declare, but errors on
  `-var`/`-var-file` for an undeclared one, which matters when one shared
  file covers directories that each only use some of the variables.
- Everything else — `.deploy-deps` discovery, topological sort with cycle
  detection, `.manual-check`/`.manual-before` handling, the WSL-aware
  `open` command, `graph`/`graph dot` — is a direct port.

## Running in CI (non-interactive)

- `--non-interactive` never reads stdin. `apply` stops at a directory whose
  `.manual-before` is not satisfied by its `.manual-check` (and skips a
  directory whose backend cannot save a plan) instead of waiting for Enter.
  Without this flag a CLI `apply` with closed stdin would step straight past a
  manual step. `plan` never prompts.
- `<dir> init` initialises a single directory, so a CI job can init only the
  stacks it is about to plan.
- On a directory that already has `deploy_backend.tf`, every `init` now passes
  the `[backend]` settings (`-backend-config`, with `{dir}` resolved), so a
  fresh checkout can init without a previous `.terraform/`. A directory without
  the stub is unchanged. `serve` builds its own runner and does not do this.
- Cloudflare R2 example for `[backend]` (needs Terraform >= 1.10 for
  `use_lockfile`; R2 has no DynamoDB):

```
[backend]
type=s3
bucket=<bucket>
key=homelab_v2/{dir}/terraform.tfstate
region=auto
use_lockfile=true
use_path_style=true
skip_credentials_validation=true
skip_metadata_api_check=true
skip_region_validation=true
skip_requesting_account_id=true
skip_s3_checksum=true
endpoints={ s3 = "https://<account id>.r2.cloudflarestorage.com" }
```

  Credentials stay out of git: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` in
  `.env`'s `[env]` section or the CI environment.

## Web dashboard

```
homelab-deploy serve [addr]     # default addr is :8080; needs auth settings (below)
```

Click Plan/Apply instead of going back and forth to a terminal, with
terraform's output streaming into the browser in real time as it runs
(Server-Sent Events) rather than staring at a spinner. `homelab-deploy
serve` starts it; visit `http://localhost:8080` (or whatever `addr` you
pass).

- **One shared codebase with the CLI.** The dashboard doesn't
  re-implement "how to plan/apply a dependency graph" — it calls the
  exact same `internal/orchestrate` functions the CLI does, with
  different `Hooks` (live SSE output instead of stdout, no interactive
  stdin prompting). They can't quietly drift apart.
- **A "Sync All" button, in multi-repo mode only** (hidden entirely
  otherwise — nothing to sync in a single-repo setup). Same job/pane/SSE
  machinery as Plan/Apply/Init, not a separate mechanism: each repo gets
  its own live pane with real git output, exactly like a directory's
  pane during `plan`/`apply`. `sync` has no per-repo targeting, in the
  UI or the CLI — it always covers every entry in `.deploy-repos`.
- **Live per-directory output**, each in its own pane, multiplexed over
  a single SSE connection per job (tagged by directory) rather than one
  connection per directory — deliberately, since browsers cap concurrent
  connections per origin and a homelab-sized dependency graph could
  plausibly hit that with one-connection-per-directory.
- **Only one job at a time.** A `POST /run` while another job is running
  gets a 409, and the dashboard shows a banner rather than silently
  queuing or racing two runs — terraform's own state locking protects a
  single directory from two concurrent applies, but nothing stops two
  browser tabs from starting two overlapping *full* runs without this.
- **A running job can be canceled** from its own page (`POST
  /jobs/{id}/cancel`) — the job's context is canceled, which
  terraform-exec propagates all the way down to the running `terraform`
  process via `exec.CommandContext`, the same mechanism the CLI's own
  `--timeout` uses. There's no gentler way to interrupt a command
  mid-step than killing it, so a canceled apply can leave a directory's
  state exactly as inconsistent as a Ctrl-C on the CLI would — that risk
  already existed, this just gives the web UI a way to take it
  deliberately instead of the job (and the single job slot) being stuck
  until the process restarts. Verified for real: an `apply` against a
  directory with an 8-second `local-exec` sleep was canceled and
  reported `job_done` with `canceled: true` in well under a second, not
  after the full 8 seconds — proof the terraform process was actually
  killed, not just marked canceled in this tool's own bookkeeping while
  it kept running underneath.
- **Dropped SSE output is surfaced, not silent.** A job's live output is
  fanned out over a bounded-size channel per subscriber and a
  bounded-size replay history (4000 frames); either one filling up
  (a very slow connection, or a very large run) means some frames get
  dropped rather than blocking a real terraform apply. When that
  happens, a `gap` SSE event reports how many frames were lost, shown as
  a banner on the job page — so a truncated or lossy view of a run's
  output looks like exactly that, not like the run simply produced less
  output than it did.
- **Clicking Plan/Apply/Init doesn't navigate away.** It redirects back
  to the dashboard, which shows a live progress bar (X / N directories
  done, updated over the same SSE stream the job page uses) with a
  "view live output" link for anyone who wants the actual terraform
  text — the job page still exists and still streams everything in
  real time, it's just no longer where you land automatically. The
  progress bar reloads the dashboard once `job_done` fires, so the
  merged results table below picks up the finished run without you
  doing anything. This ordering is deliberate and was a real bug at one
  point: `job_done` fires the instant `finish()` runs, so
  `report.html`/`report.json` are written to disk *before* that,
  in-goroutine, not after — otherwise there'd be a genuine window where
  a client sees "done" (and the dashboard reloads) before the report
  it's about to show actually exists. Caught from real local test output
  (a `t.TempDir()` cleanup racing a still-writing background goroutine,
  not something the sandbox environment happened to hit), fixed by
  reordering, and pinned down with a test asserting the report exists
  at the exact moment `job_done` arrives over SSE — rather than only
  inferring the fix from incidental timing.
- **Manual steps (`.manual-before`) are handled safely, not
  interactively** — a web request can't pause and wait for someone to
  press Enter the way the CLI does. A directory needing a manual step
  proceeds only if a `.manual-check` already reports it satisfied;
  otherwise that directory's live pane explains what's needed and how to
  finish it from the CLI, and the run stops there (matching the CLI's
  own stop-on-first-failure behavior for `apply`).
- **One merged table, not two.** The last result (status, change counts,
  and the same attribute-level diff the static report shows, e.g.
  `input: "v1" → "v2"`) and the Plan/Apply/Init buttons live in the same
  row per directory — not a separate results table underneath
  duplicating the directory list. A directory the last job didn't touch
  (e.g. you ran a single-directory plan) just shows `—` rather than
  stale or misleading data from an earlier run. Dependency info isn't
  duplicated here — that's what the static report's graph tab is for.
- **Filter by status**, pure CSS (same hidden-radio + sibling-selector
  trick as the tabs elsewhere) — All / OK / Changes / Error / No result,
  no JavaScript, no page reload.
- **A full run (Plan All / Apply All) writes the same static
  `.deploy-logs/report.html`/`report.json`** the CLI produces — graph tab
  and all. A single directory's own button does **not** touch that
  shared file (it would otherwise clobber the full multi-directory report
  with a one-directory view) — its result lives on the live job page
  (and, for whichever job ran most recently, on the dashboard itself).
- **Authentication and roles: see the next section.** `serve` does not
  start without it, unless you pass `--insecure-no-auth`.
- **Logs what it's actually doing.** Every request gets one access-log
  line (method, path, remote address, status, duration), and `/run`
  specifically logs who asked (`user="alice"`) and what for (`kind=plan dir="services/a"`) and
  the outcome (`job started`/`job finished ... ok`/`FAILED: ...` or
  rejected with why) — so running `serve` in the foreground, or checking
  its logs under systemd/Nomad, actually shows what the browser is
  asking the server to do rather than staying silent until something
  breaks. SSE connect/disconnect gets its own line too, since the
  generic access-log line for a stream only prints once the (possibly
  long-lived) connection finally closes.
- **The one place this tool uses JavaScript at all.** Small, vanilla,
  no build step, no dependencies — `EventSource` for the SSE connection
  and plain DOM updates for the live panes. There's no way to consume a
  server push into a specific part of a live page with CSS alone, unlike
  the static report's tabs/hover-highlight, which stay pure CSS since
  they don't need to react to anything happening after the page loads.

### Authentication and roles

`serve` signs users in itself with **OpenID Connect** (authorization
code flow with PKCE), so it needs no auth proxy in front of it. It works
with any provider that publishes standard OIDC discovery
(`<issuer>/.well-known/openid-configuration`) and supports the
authorization code flow. Nothing provider-specific is assumed; the claim
names are settings. (The test suite runs against a fake provider that
implements the protocol; it has not been run against a specific
product, so report any provider that needs a different setting.)

Flow: an unauthenticated browser is redirected to `/auth/login`, then to
the provider, and back to `/auth/callback`. The server checks the ID
token (signature against the provider's published keys, issuer, audience
= your client id, expiry, and the login's nonce), reads the user's
groups from a claim, and starts a session (an `HttpOnly`,
`SameSite=Lax` cookie; over https it is `Secure` and named `__Host-hd_session`). Sessions
are held in memory, so a restart signs everyone out.

| Role | Can |
|---|---|
| `viewer` | the dashboard, `/graph`, job pages and their live output (`/jobs/{id}`, `/jobs/{id}/stream`), the static report (`/logs/report.html`, `/logs/report.json`; nothing else under `.deploy-logs`) |
| `planner` | everything a viewer can, plus `plan`, `init` and `sync` runs, and cancelling those jobs |
| `applier` | everything a planner can, plus `apply` runs, **confirming** an apply (`/jobs/{id}/confirm`), and cancelling an apply job |

Roles are cumulative. A user's role is the highest one any of their
groups maps to; groups are matched exactly (case-sensitive, untrimmed).
Roles are read **at login**, so a group change takes effect the next time
the user signs in, or when their session expires (`HOMELAB_DEPLOY_SESSION_TTL`
is the upper bound; shorten it if that is too long). If a user who has lost
all their groups tries to sign in, every session that account holds is
revoked immediately.

**The planner role is not a sandbox.** It limits what the buttons do, not
what Terraform does. `plan` and `apply` use the same runner and the same
credentials, and Terraform runs provider and data-source code (for example
an `external` data source) at plan time, and `init` downloads providers and
modules. Anyone who can change the repository being planned, or the repos
`sync` pulls, can therefore run code with this server's credentials through
a planner's `plan`/`init`/`sync`, without ever holding the applier role. Give
`planner` only to people you would trust with that, and treat write access
to the repos as equivalent to apply access. If that is not acceptable, the
fix is separate read-only credentials for planning, which this server does
not do.

Configure it with environment variables:

| Variable | Meaning |
|---|---|
| `HOMELAB_DEPLOY_OIDC_ISSUER` | **Required.** The provider's issuer URL, exactly as it appears in the `iss` claim. Must be https (http only for localhost). Authentik uses a per-application issuer: `https://auth.example.com/application/o/<app-slug>/` (keep the trailing slash). |
| `HOMELAB_DEPLOY_OIDC_CLIENT_ID` | **Required.** |
| `HOMELAB_DEPLOY_OIDC_CLIENT_SECRET_FILE` | Path to a file containing the client secret (never the secret itself, so it stays out of env and process arguments). Unset = a public client using PKCE alone, if the provider allows it. |
| `HOMELAB_DEPLOY_PUBLIC_URL` | **Required.** The address browsers use to reach this server, as just `scheme://host[:port]`, e.g. `https://deploy.example.com`. The provider's redirect URI must be registered as this plus `/auth/callback`, in canonical form: lower-case host, no default port (`https://deploy.example.com/auth/callback`, not `...:443/...`). Must be https (http only for localhost). |
| `HOMELAB_DEPLOY_VIEWER_GROUPS`, `..._PLANNER_GROUPS`, `..._APPLIER_GROUPS` | Comma-separated group names mapped to each role. At least one must be set. Group names containing a comma cannot be expressed. |
| `HOMELAB_DEPLOY_OIDC_GROUPS_CLAIM` | ID-token claim holding the groups, default `groups`. A JSON array of strings, or a single string. Nested claims (e.g. Keycloak's `realm_access.roles`) are not supported; use a mapper to emit a top-level claim. |
| `HOMELAB_DEPLOY_OIDC_USERNAME_CLAIM` | Claim used as the name in logs, default `preferred_username`, then `email`, then `sub`. |
| `HOMELAB_DEPLOY_OIDC_SCOPES` | Space or comma separated, default `openid profile`. `openid` is always requested. Add whatever scope your provider needs to put groups in the token. |
| `HOMELAB_DEPLOY_SESSION_TTL` | Session lifetime, default `8h`. |

On the provider side: create a confidential OIDC client with the
authorization code grant, the redirect URI above, and make sure the ID
token carries the groups claim. In Authentik that is the `profile` scope on
recent versions; on others it needs a scope mapping that returns
`{"groups": [...]}`, so check the ID token your version issues.

**It fails closed.** `serve` exits at startup if any required setting is
missing, a URL is not https, no group maps to a role, or the provider's
discovery document cannot be fetched. `--insecure-no-auth` is the only
way to run without auth; it logs a warning and is refused if any of the
variables above are also set. (This is a change from earlier versions,
where `serve` always started open.)

**Transport.** The server itself speaks plain HTTP (`http.ListenAndServe`)
and relies on a TLS-terminating reverse proxy for https. The `Secure` cookie
flag only protects the browser-to-proxy leg. The proxy-to-server connection
must therefore be local to the host or otherwise encrypted and private (a
loopback or private container network at least): anything that can read it
sees session cookies, which can be replayed until they expire. Likewise
`HOMELAB_DEPLOY_PUBLIC_URL` must be the https address the browser uses, or
the cookies lose `Secure` and the `__Host-` prefix.

Other protections:

- A state-changing request (`POST`) must be same-origin: the browser's
  `Sec-Fetch-Site: same-origin`, or an `Origin` equal to the public URL
  (compared as scheme, lower-cased host and effective port); with neither
  it is refused. This is the CSRF defence, since the session cookie is
  sent automatically.
- Every response sends `X-Frame-Options: DENY`, `frame-ancestors 'none'`
  and `Cache-Control: no-store`, so the pages (which have Apply buttons)
  cannot be framed by another site, even a sibling subdomain, or kept by a
  shared cache.
- Over https the cookies are `__Host-` prefixed (`Secure`, `Path=/`, no
  `Domain`), so another service under the same parent domain cannot plant
  its own session or login state in your browser.
- The login `state` is bound to the browser by a cookie and is single
  use (checked and consumed atomically); the redirect after login (`next`)
  can only be a short path on this server, with no backslashes.
- The discovered authorization, token and JWKS endpoints must be https
  (loopback excepted), and the HTTP client used for discovery, JWKS and the
  token exchange refuses redirects to anything else. Only asymmetric
  signing algorithms are accepted for ID tokens.
- Group names from the token are matched exactly, with no trimming.
- Only `report.html` and `report.json` are served from `.deploy-logs`:
  saved plan files there can contain unmasked secret values.
- An open job stream ends when its session does (logout or expiry,
  checked every few seconds), not only at connect time.
- The one-time authorization code and state are kept out of the logs, and
  untrusted values in log lines are quoted.
- A new login replaces the session the browser presented, so re-login
  really drops the old privileges. One account holds at most 5 sessions
  (a few devices; the oldest is dropped), so a single account cannot fill
  the store. Sessions are capped at 1000 overall; at the cap a new login is
  refused rather than logging someone else out.
- Two logins started in different tabs share one login cookie: the second
  start replaces the first, and the first tab's callback then fails and
  has to be restarted. A stale callback never destroys the other login. Pending logins are capped at 1000 too, and
  when full the oldest abandoned one is dropped, so a flood of
  `/auth/login` requests cannot lock real users out (it can still churn
  them; sustained flooding needs rate limiting at the proxy, since the
  server cannot tell clients apart behind it).

Every action is logged with `user` (the display claim, which providers
do not guarantee to be unique or permanent) **and `sub`**, the provider's
stable subject id, so two accounts with the same name can be told apart.
Every run request is logged with the user, kind, directory and outcome
(`run denied` / `run rejected` / `job started id=...`; the later `job
finished id=...` line carries the same id), plus logins, denied logins
and denied requests with the reason. Client-supplied values (directory
names) are logged quoted, so they cannot forge a log line. `POST
/auth/logout` ends a session.

Not covered yet: the dashboard does not show who is signed in or offer a
logout button, and does not hide Apply from users who lack the role
(they get a 403 page). Non-browser clients (scripts, an MCP server) cannot
authenticate; `Auth.identify` is where a bearer-token check would plug in.

## Files this creates

- **`.terraform.lock.hcl`** (one per directory) — normal Terraform
  behavior, unrelated to this tool, created by `terraform init`. It pins
  provider versions/hashes; HashiCorp recommends committing it.
- **`.deploy-logs/<slug>.tfplan`** — this tool's own saved plan files, see
  above. Gitignore `.deploy-logs/` (same as the original script's
  logs/report output).
- **`deploy_backend.tf`** (only in directories you've run
  `init --migrate-backend` against) — the generated backend-type stub.
  Unlike the other files here, **commit this one**: see "Remote backend"
  above for why.
- **`.deploy-repos-checkouts/`** (multi-repo mode only) — every
  git-managed entry's actual checkout. Gitignore this whole directory;
  see "Multi-repo mode" above.

## Layout

```
main.go                       CLI entry point (init/plan/apply/graph/open/serve/<dir>)
internal/orchestrate/orchestrate.go  plan/apply/init orchestration shared by the CLI and the web server
internal/graph/graph.go       directory discovery + topological sort
internal/graph/manual.go      .manual-check / .manual-before handling
internal/graph/render.go      text/DOT graph rendering + Layout() for the HTML graph tab
internal/tf/env.go            .env parsing ([tfvars]/[env] sections, TF_VAR_ auto-prefix, quote-stripping)
internal/tf/tf.go             terraform-exec/terraform-json wrapper, optional live-output tee
internal/tf/diff.go           attribute-level before/after diff extraction (sensitive masking, unknown-after-apply)
internal/tf/backend.go        backend stub generation + migration ({dir} placeholder, -force-copy)
internal/manifest/manifest.go .deploy-repos parsing (git URL detection, ref, tf-root subpath, local paths)
internal/manifest/resolver.go the one seam: node name -> real filesystem path, single- or multi-repo
internal/manifest/sync.go     git clone/fast-forward-update, dirty/diverged refusal, local-exclude fix
internal/report/report.go     HTML (html/template, incl. the SVG graph tab) + JSON report generation
internal/report/open.go       cross-platform / WSL-aware report opener
internal/server/server.go     HTTP routing, the single-job-at-a-time lock
internal/server/job.go        one plan/apply/init run: SSE event publishing, manual-step handling
internal/server/broadcaster.go  generic SSE fan-out with replay for late subscribers
internal/server/handlers.go   dashboard / start-run / job-page / stream handlers
internal/server/templates.go  dashboard + live job page HTML (incl. the SSE client JS)
```

## Tests

```
go test ./...              # add -race to also run under the race detector
```

210 tests across seven packages (the top-level CLI package,
`internal/graph`, `internal/manifest`, `internal/orchestrate`,
`internal/report`, `internal/server`, `internal/tf`) — the top-level package's tests are
`parseFlags` coverage (working-dir/env-file/verbose/timeout parsing,
long/short/`=`-form flags, interspersed flags-after-the-subcommand
parsing, `--help`). Most of the rest are pure-logic unit tests needing
no `terraform`
binary: topo sort/cycle detection, dependency-depth layout for the graph
tab, `.manual-check`/`.manual-before` evaluation, the `.env` parser
(including quote-stripping and the default-section behavior), plan
summarization (`terraform-json` `Actions` → create/update/delete/replace),
report status classification and sorting, the SVG graph builder (node
coloring by status, manual-step markers, edge direction, and the
hover-highlight wiring — node IDs, per-node `conn-*` classes on
neighbors/edges in both directions, isolated nodes correctly getting no
`:has()` rule, slug collision handling), HTML auto-escaping, JSON
round-trip.

Two things specifically need a real `terraform` binary and skip
themselves automatically if one isn't on `PATH` — no build tag or flag
needed:

- `internal/tf/integration_test.go` drives a real
  `terraform init/plan/apply` lifecycle (create → no-op → update →
  delete) through the `Runner`, checking the actual structured
  `PlanResult` at each stage rather than mocking terraform-exec's output.
  It also verifies the live-output tee (`liveOut`) actually receives
  real terraform output as it's produced, not just that `RawOutput`
  still builds correctly — that's what the web server's SSE streaming
  depends on. This is also what caught a real bug: `ShowPlanFile`
  internally runs its own `terraform show -json` through the same
  client, whose output was silently swallowed for `RawOutput` (the
  string snapshot happens before it runs) but would have leaked into
  `liveOut` as JSON noise — fixed by explicitly discarding that call's
  output rather than leaving it attached to the shared writer.
- `internal/orchestrate/orchestrate_test.go` tests `PlanAll` (concurrent
  plan, including that `OnDirDone` is never called concurrently for two
  directories even though the plans themselves run in parallel),
  `ApplySequential` (stops at the first failure, never touches
  directories after it, the default non-interactive manual-step handler
  blocks correctly and a supplied hook can override it), and `InitAll`
  — all against real terraform. Also an informational timing comparison
  (concurrency=1 vs the default) — informational because timing is
  inherently environment-dependent; in a single-core sandbox with no
  real provider network calls it can show no benefit or even a small
  regression (pure CPU-bound context-switching overhead with nothing to
  overlap). The scenario this feature targets is I/O-bound provider API
  calls (Nomad, Vault, TrueNAS, Authentik, ...) across many directories,
  where a blocked network wait in one goroutine doesn't stop others from
  progressing — not something a single-core, no-network sandbox can
  demonstrate, only correctness (which `-race` and the index-based-
  ordering assertions do cover).

Both use the `terraform_data` resource, built into Terraform core (1.4+),
specifically so they need zero network access or provider download to
run.

I verified all of this for real (not just compiled) in my sandbox by
installing OpenTofu — Terraform's MPL-licensed, CLI-compatible fork — as
`terraform` on `PATH`, since HashiCorp's own release/registry domains
aren't reachable from here, then generated an actual multi-directory
report (a dependency chain, a manual step, a deliberate error) and parsed
the embedded SVG back out as XML to confirm it renders correctly — node
IDs, `conn-*` classes on the right neighbors, the generated `:has()` CSS
referencing the right node IDs, the isolated (no-dependency) directory
correctly getting no hover rule at all — rather than just eyeballing it.

One genuine finding came out of the integration testing: a config that
uses *only* the built-in provider never gets a `.terraform/` directory
from `init` (there's nothing to download), so the directory-existence
check `IsInitialized` uses — inherited unchanged from the original bash
script's `[[ -d "$dir/.terraform" ]]` — can't distinguish "not
initialized" from "initialized, nothing to fetch" for that one degenerate
case. Doesn't affect you in practice (every real directory here pulls in
an actual provider and will always produce `.terraform/`), and
`TestIsInitialized`/`TestIsInitialized_FileNotDirIsNotInitialized` in
`tf_test.go` pin down the check's actual behavior directly rather than
relying on a real `terraform init` to exercise it.

Not covered: `main.go`'s CLI argument parsing/dispatch and
`internal/report/open.go`'s WSL detection — both need a real
OS/terminal environment to exercise meaningfully rather than a scratch
Terraform config.

**`internal/server`** has its own tests covering both layers: pure unit
tests for `Broadcaster` (publish/subscribe/replay/close, history
capping, and — under `-race` — that a slow/unread subscriber gets
frames dropped rather than blocking the publisher, since a laggy browser
tab must never be able to stall a real terraform run), and HTTP-level
integration tests via `httptest` against real terraform: the dashboard
lists real directories, an unknown directory or invalid job kind is
rejected, starting a second job while one's running gets a 409, and —
the actual point of this feature — a real SSE stream is read end-to-end
and asserted to contain `output`, `status`, and `job_done` events, plus
a late subscriber (joining after the job already finished) still gets
the replayed events. I additionally drove the whole thing manually
through `curl` against a live running server — starting jobs, watching
real terraform plan/apply output arrive over SSE, confirming the 409
lock and its release, confirming a full run writes the shared static
report while a single-directory click doesn't touch it, and confirming
a blocked manual-step directory explains itself in its live pane and
correctly stops the run before reaching a directory that depends on it
— since some of this (does a real browser's `EventSource` experience
actually see the events land, does the process survive across separate
requests) is more convincing exercised for real than asserted in Go.

The `sync` job kind's tests clone a real repo (a real bare git repo,
same offline-hermetic approach as `internal/manifest`'s own tests) via
an actual `POST /run` + SSE flow — the dashboard's "Sync All" button,
end to end, not just the underlying `manifest.Sync` function in
isolation. Writing this one caught a real bug, worth being honest about
even though it turned out to be in the test rather than the feature:
the first version intermittently failed, and initial debugging pointed
toward something git- or timing-related (a standalone reproduction
outside the HTTP layer succeeded reliably, which briefly looked like it
implicated the server wiring specifically). The actual cause was
simpler and my own mistake — the test polled the dashboard for the
literal text `"already running"`, which the progress-bar redesign had
already replaced several iterations earlier; every other test that
needed this check had been updated to look for `id="progress-notice"`
instead, this one was just new and got missed. Fixed by using the
existing correct `waitForJob` helper, then confirmed genuinely fixed
(not just coincidentally passing) across 8 repeated isolated runs.

## Releases

`.github/workflows/release.yml` publishes a compiled binary so pipelines can
download it instead of building it.

**Cut a release** (after the commit's CI is green on `main`):

```
git tag v0.1.0 <commit>
git push origin v0.1.0
```

The workflow refuses to publish if `ci.yml` has no successful run for the
tagged commit. It builds `linux/amd64` (`CGO_ENABLED=0`, `-trimpath`, from a
clean Go build with the version in `go.mod`), then attaches
`homelab-deploy_<tag>_linux_amd64.tar.gz` and `SHA256SUMS` to the release.
Other platforms are not built; add them to the build step if needed.

**Consume it in a pipeline** (the repo and its releases are public, so no
token is needed). Pin the tag, verify the checksum, never use "latest":

```
TAG=v0.1.0
BASE="https://github.com/byteford/deploy/releases/download/$TAG"
curl -fsSLO "$BASE/homelab-deploy_${TAG}_linux_amd64.tar.gz"
curl -fsSLO "$BASE/SHA256SUMS"
sha256sum -c SHA256SUMS
tar -xzf "homelab-deploy_${TAG}_linux_amd64.tar.gz" homelab-deploy
```

Note the checksum file is published next to the binary, so it detects a
corrupted download, not a compromised release. Rely on pinning the tag and on
restricting who can push tags and write to releases for that.

### Container image

The same workflow builds a container image once the release has been
created (the `image` job runs after `release`; it is a job rather than a
separate workflow because a release made with `GITHUB_TOKEN` does not
trigger other workflows). A `v*` tag pushes:

- `ghcr.io/byteford/deploy:<version>` (the tag without the leading `v`)
- `ghcr.io/byteford/deploy:latest`, except for pre-releases such as `v1.2.3-rc1`

A PR touching `Dockerfile`, `.dockerignore` or `release.yml` builds and
smoke-tests the image without pushing it.

The image is `alpine` with `homelab-deploy`, `terraform` (the **latest**
release at build time, copied from `hashicorp/terraform:latest`; pin it in
the Dockerfile if a newer Terraform ever breaks a run), `git` and an `ssh`
client (for `sync`). It runs as uid 10001 in `/work`, listens on `:8080`
and starts `homelab-deploy serve :8080` by default.

```
docker run --rm -p 8080:8080 -v "$PWD":/work ghcr.io/byteford/deploy:<version>
```

Things to know:

- Mount the repo to operate on at `/work`, writable by uid 10001
  (`.deploy-logs/` is written there). Secrets (`.env`, backend credentials,
  an SSH key for `sync`) come in at run time as environment variables or
  mounted files; the build context's `.env*` files are excluded by
  `.dockerignore` and never enter a layer.
- The binary is built from the tagged commit with the release workflow's
  flags, but it is a separate build from the release tarball, not that
  file.
- `latest` is a moving tag and so is the Terraform inside it. Pin the
  version tag when deploying.
- A package pushed to ghcr.io starts private; making it public (or giving
  the puller a token) is a package setting on GitHub.

## CI

`.github/workflows/ci.yml` runs on every push/PR to `main`: build, `go
vet`, a `gofmt -l` formatting check, and the full test suite with
`-race`, against a real `terraform` binary (`hashicorp/setup-terraform`,
with `terraform_wrapper: false` — its default wrapper script captures
stdout/stderr for its own logging, which would interfere with
terraform-exec needing the raw output directly). Unlike my sandbox here,
a GitHub Actions runner has normal internet access, so this installs
actual HashiCorp Terraform rather than needing OpenTofu as a stand-in —
every test that skips itself here for lack of `terraform` on `PATH` runs
for real in CI.

Verified the workflow itself, not just written it: linted with
`actionlint` (including its `shellcheck` integration for the embedded
bash) with zero issues, and confirmed every action reference against
each project's actual GitHub releases page rather than trusting
possibly-stale training data — `actions/checkout@v7`,
`actions/setup-go@v6`, and `hashicorp/setup-terraform@v4` are the real
current major versions as of this writing. Also ran the exact sequence
of commands the workflow runs, end to end, against a fresh copy of the
repo, and it passes clean.

## Extending it later

A few concrete next steps that this structure makes easy:

- **Notifications**: after `writeReports` in `main.go`, read `rep.Entries`
  and POST a summary to a webhook when any `Status == StatusChanges` or
  `StatusError`.
- **Parallel apply of independent branches**: `apply` is still fully
  sequential in `g.Order` (unlike `plan`, which is already concurrent —
  see above). `graph.Layout`'s `Depth` field already gives you the BFS
  layering needed to apply each depth level's directories concurrently
  while still waiting for the previous level to finish.
- **`--only <dir>` / `--skip <dir>` filtering**: filter `g.Order` before
  the loop in `cmdFullRun`.
- **Drift detection as a scheduled job**: `cmdFullRun(repoRoot, "plan")` is
  already a self-contained function — call it from a cron-triggered binary
  or a Nomad periodic job and alert on `StatusChanges`/`StatusError` via
  the JSON report.

## License

MIT, see [LICENSE](LICENSE).
