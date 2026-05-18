# bd-fast — patched beads CLI for gascity

We fork `gastownhall/beads` and strip per-command overhead specific to our
workflow (server-mode Dolt, single-project, no JSONL sync, no Linear).
Result: **bd is 3-12× faster** than the upstream release on our setup.

This doc is the working notes + maintenance handbook. For raw bench output
see `.runtime/bench/SUMMARY.md`. For research notes that led here, see
"Investigation history" below.

## TL;DR — current state (2026-05-17)

| | |
|---|---|
| Fork | https://github.com/npavlov/beads (branch `gascity-fast`) |
| Source clone | `/Volumes/DATA/repos/personal/beads` |
| Installed binary | `/opt/homebrew/bin/bd-main` |
| Default `bd` | `~/.local/bin/bd` → symlink → `bd-main` |
| Version reported | `bd version 1.0.4-fast (<sha>-v7)` |
| Upstream baseline | `bd 1.0.4` (brew, vanilla) |

## Why this exists

### The problem
Stock `bd` on our setup ran every command in 1-3 seconds. Under gc-supervisor
load (mayor / orders / dispatchers each invoking bd 10s of times per minute),
this manifests as:

- `building_city_runtime` hitting 3-min ceiling consistently
- Order dispatcher tick wedging — only the highest-frequency order (30s)
  makes it through; others fall hours behind
- `gc converge create` printing `controller did not respond in time`
- Sessions taking 25-30s per spawn

### The root causes (found in upstream codebase)
Two classes of issues:

1. **Known upstream bugs.**
   - [`gastownhall/beads#3849`](https://github.com/gastownhall/beads/issues/3849)
     — server-mode auto-import has no emptiness guard. Every `bd update`
     re-imports the whole `issues.jsonl` even when DB is populated. 9s per
     write upstream. Fixed in `main` via [PR #3691](https://github.com/gastownhall/beads/pull/3691),
     **not yet in any release** — `v1.0.4` (latest) shipped 7 hours before
     the fix merged. See [`#3870`](https://github.com/gastownhall/beads/issues/3870)
     asking for `v1.0.5` cut.

2. **Per-command overhead accumulated in `main` HEAD.** Stripping is the
   only way to recover speed. Vanilla HEAD is 2× SLOWER than `v1.0.2` on
   our setup because of:
   - `auto-migrate` schema check on every call
   - `auto-backup` aggressive retry loop
   - `auto-export` throttle check
   - More extensive `bd doctor`-style health checks

   None of these are configurable to "off" upstream; they fire from
   `PersistentPreRun` / `PersistentPostRun`.

## What we did

Forked `gastownhall/beads` at HEAD (`74c66722`) and applied 12 surgical
short-circuits + 1 new command. Each patch is a single `return` near the top
of a function, gated by `gascity-fast:` comment. Reversible per-commit.

### Strips (per-command no-ops)

| Patch | What it disables | Why safe for gascity |
|---|---|---|
| `cmd/bd/backup_auto.go` | `maybeAutoBackup` (`PersistentPostRun`) | We deleted the broken backup destination earlier; gascity has no use for autoperiodic backups |
| `cmd/bd/export_auto.go` | `maybeAutoExport` JSONL writer | We're server-mode; dolt is source of truth, JSONL is dead weight |
| `cmd/bd/auto_import_upgrade.go` | `maybeAutoImportJSONL` probe | Even with #3691 fix, runs `GetStatistics()` DB query just to early-return; we don't use the upgrade path |
| `cmd/bd/worktree.go` | `warnMultipleDatabases` | `FindAllDatabases()` walks dir hierarchy; we have single `.beads/` |
| `cmd/bd/main.go` molecule call site | `molecules.NewLoader().LoadAll()` | Gated behind `GASCITY_LOAD_MOLECULES=1` env. Walks town/user/project paths + upserts builtins per command. gc formulas live in `gascity/formulas/`, not bd's molecule loader |
| `cmd/bd/main.go` `getActorWithGit()` | `git config user.name` subprocess | Falls through to `$USER` env. Saves ~50-100ms per call on macOS. Set `BEADS_ACTOR` if you want a literal name |
| `cmd/bd/version_tracking.go` | `trackBdVersion` | We pin a custom binary; version-upgrade detection is moot |
| `cmd/bd/main.go` `validateWorkspaceIdentity` | DB metadata read on every write | Fires `GetMetadata("_project_id")` to detect drift between metadata.json and DB; single-project setup |
| `cmd/bd/tips.go` | `maybeShowTip` | Iterates tip registry with per-tip DB metadata reads; called from create/list/show/ready |
| `internal/storage/hook_decorator.go` | `fireHookByID` post-mutation re-fetch | Was firing 1 extra `GetIssue` DB roundtrip per write to pass full Issue to hook scripts. Now passes minimal `{id}` stub; hook scripts that need full state can `bd show $1 --json` |
| `cmd/bd/dolt_autopush.go` | `maybeAutoPush` hardcoded off | Defense-in-depth — config `dolt.auto-push` is already `off`, but the binary now refuses to push regardless. Explicit `bd dolt push` still works |
| `cmd/bd/sync_remote.go` | `resolveSyncRemote` / `resolveSyncRemoteFromDir` always return `""` | Defense-in-depth — even if `sync.remote: "..."` ends up in `.beads/config.yaml`, bootstrap/hooks/auto-push paths take the no-remote branch. Stops accidental git+ssh push to upstream |

### New command

- `cmd/bd/nuke.go` — `bd nuke` wipes all user-data tables (`DELETE FROM` each),
  preserves schema/config tables. Required `--force` to skip the
  `type DELETE` confirmation. Optional `--full` also wipes `metadata` table
  (project_id, repo_id, clone_id).

## Benchmark results

Measured N=20 on hq dolt @ ~690 KB JSONL, `auto-commit: batch`, single user.

| Op | brew v1.0.4 | **bd-fast v7** | improvement |
|---|---:|---:|---:|
| `bd show <id>` | ~3000 ms | 276 ms | 11× |
| `bd ready` | ~3100 ms | 326 ms | 10× |
| `bd list --json --limit 100` | ~2700 ms | 295 ms | 9× |
| `bd update --set-metadata` | ~3800 ms | 728 ms (warm: ~450 ms) | 5× cold / 8× warm |

Raw N=20 data: `.runtime/bench/final-v7-N20-*.txt`. Iteration-by-iteration
history (v1→v7): `.runtime/bench/SUMMARY.md`.

CPU profile of `bd update` shows **100% time in `runtime.kevent`** (Go I/O
multiplexer wait). The remaining cost is pure dolt SQL roundtrip latency
— stripping more CLI code won't help.

## Operational handbook

### Verify current install
```bash
which -a bd                      # should show ~/.local/bin/bd first
bd --version                     # should show "1.0.4-fast (<sha>-vN)"
ls -la ~/.local/bin/bd           # should be symlink to /opt/homebrew/bin/bd-main
```

### Rebuild after pulling new patches
```bash
cd /Volumes/DATA/repos/personal/beads
git checkout main                # main = gascity-fast tip (always ff-merged)
git pull origin main
make gascity-fast-build          # build + codesign + install in one step
                                 # default: FAST_TAG=v7, FAST_INSTALL_DIR=/opt/homebrew/bin
bd --version                     # confirms install
```

Bump the version tag when adding new patches:
```bash
make gascity-fast-build FAST_TAG=v8
```

Manual build (if you need to override anything not covered by the Makefile):
```bash
CGO_ENABLED=1 GOTOOLCHAIN=auto go build \
  -tags "gms_pure_go" \
  -ldflags="-X main.Version=1.0.4-fast-v7 -X main.Build=$(git rev-parse --short HEAD)" \
  -o ./bd ./cmd/bd
codesign -s - -f ./bd
cp ./bd /opt/homebrew/bin/bd-main
```

### Sync from upstream (cherry-pick or rebase)
```bash
cd /Volumes/DATA/repos/personal/beads
git fetch upstream
git checkout gascity-fast
git rebase upstream/main         # may have conflicts — patches are tiny, usually clean
git push origin gascity-fast --force-with-lease
# then rebuild as above
```

### Rollback to brew bd
```bash
rm ~/.local/bin/bd               # removes symlink; PATH falls through to /opt/homebrew/bin/bd
bd --version                     # back to "1.0.4 (Homebrew)"
```

### Bench after changes
```bash
cd /Volumes/DATA/repos/taxdome/taxdome/gascity
TS=$(date +%Y%m%dT%H%M%S)
LABEL=after-changes-N20 BD=/opt/homebrew/bin/bd-main N=20 \
  .runtime/bench/run.sh | tee .runtime/bench/after-changes-N20-$TS.txt
# Compare numbers against final-v6-N20-*.txt
```

### Add a new strip patch
1. Find the per-command function in `cmd/bd/*.go` or `internal/storage/*.go`
2. Add early `return` at top with `// gascity-fast: <reason>` comment
3. Commit on `gascity-fast` branch
4. Rebuild + install + bench

## Trade-offs accepted

These behaviors are gone. If you need them, revert the corresponding commit.

- No automatic backup to `.beads/backup/`
- No automatic `issues.jsonl` export
- No `issues.jsonl` upgrade-recovery import
- No "multiple databases detected" warning
- No `git config user.name` lookup for actor (uses `$USER`)
- No bd-version-upgrade auto-migration tracking
- No workspace identity drift check on write commands
- No educational tips
- Molecule template loader gated behind `GASCITY_LOAD_MOLECULES=1` env
- Hook scripts (`.beads/hooks/on_*`) receive minimal Issue payload — only
  the `id` field. Scripts that need title/description/etc. must
  `bd show $1 --json` to fetch.
- **No auto-push to dolt remote** regardless of `dolt.auto-push` config
- **`sync.remote` is hardcoded ignored** — even if it appears in config.yaml
  (e.g. `bd init` in a git-tracked dir re-creates it), bootstrap/hooks
  treat it as unset. To intentionally push, use `bd dolt push` explicitly.

The gascity `on_update`, `on_create`, `on_close` hooks read `title` from
the JSON stdin for the gc-event log message; with bd-fast that title is
empty string. Events still fire correctly; only the log message text is
affected. Not breaking; if you want full fidelity, revert the
`HookFiringStore.fireHookByID` commit.

## What to do next

Listed by ROI per effort. We've harvested all the CLI-level wins; the
remaining frontiers are at the I/O layer.

### 1. Unix socket instead of TCP loopback (Low effort, ~50-100 ms / call)

`bd` and `dolt` both speak MySQL protocol. TCP loopback handshake costs
~50-80 ms per call. Unix socket is dramatically faster.

**Steps:**
1. Edit `.beads/dolt/config.yaml`, uncomment + set:
   ```yaml
   listener:
     socket: /tmp/dolt-gascity.sock
   ```
2. Restart dolt: `bd dolt stop && bd dolt start`
3. Edit `.beads/config.yaml`, add:
   ```yaml
   dolt.server-socket: /tmp/dolt-gascity.sock
   ```
4. Bench: should see ~50ms shaved from every bd command.

Risk: low. If dolt doesn't accept the socket path, revert config.

### 2. Investigate residual ~370 ms cold-cache floor (Medium effort)

Even after all strips, `bd update` floors at ~300-700 ms cold. Breakdown
from `time` + OTel:
- Process startup (Go runtime + cobra + viper config): ~60-100 ms
- MySQL handshake: ~80 ms (Unix socket would shave this)
- SQL queries (GetIssue + UpdateIssue): ~100 ms
- Hook fork + script bootstrap: ~50-95 ms
- Store close + cleanup: ~30 ms

Open questions worth profiling:
- Why is write-mode store opening ~100 ms slower than read-only? (We
  observed it but didn't track down which Dolt setup step is responsible.)
- Can config loading be lazified? Viper does several file walks.
- Hook script shell init: 95 ms for a bash script with `cat`/`grep`/`printf`
  seems high — is `/bin/sh` slow to launch on Apple Silicon?

### 3. Make hook firing truly async (Medium effort, ~50-95 ms write)

`HookFiringStore.fireHookByID` already spawns the hook in a goroutine, but
something keeps bd waiting (~50-95 ms wall time attributable to hooks
even though `Run()` returns immediately). Likely culprits:
- Go's OTel SDK may flush pending spans on exit
- The fork+exec itself blocks in the goroutine until exec(2) returns
- Hook script's pre-`&` sync portion (cat + grep + printf in bash)

Options:
- Use `os.StartProcess` directly instead of `cmd.Start` + goroutine
- Re-shape gascity hooks to background EVERYTHING immediately (e.g. wrap
  the entire body in `(...) &` rather than just the final gc-call)
- Add an env var `BD_FIRE_AND_FORGET_HOOKS=1` that skips the cmd.Wait
  goroutine entirely

### 4. Batch GetIssue + UpdateIssue into one TX (Medium effort, ~30-50 ms write)

`bd update` currently does:
1. `resolveAndGetIssueWithRouting` → 1 query to fetch current state
2. Merge updates in memory
3. `UpdateIssue` → 1 query to write back

Both could share a transaction (1 connection, less roundtrip overhead).
Need a new `UpdateIssueWithCurrent` storage API or a tx-based helper.

### 5. `bd serve` daemon (Very high effort, ~200-300 ms / call)

Upstream proposal: [beads#3760](https://github.com/gastownhall/beads/issues/3760).
Author's prototype claimed 41 conn/s → 0.4 conn/s and Dolt CPU 475% → 50%
on a 4-agent setup.

Approach: long-running `bd serve` listens on Unix socket. `bd` invocations
forward to it via `BEADS_DAEMON_SOCKET`. Daemon maintains per-workspace
store registry with warm connections.

This is the only path below ~200 ms per command. But it's invasive:
- `bd` needs forward-request mode
- daemon needs request multiplexing + per-command flag/state isolation
- need lifecycle management (start with gc supervisor, drain on stop)
- need careful os.Exit handling — daemon dies if any handler calls os.Exit

Considered out of scope for one person.

### 6. Lazy config init (Low-medium effort, ~30-50 ms / call)

`config.Initialize()` is called from `prepareSelectedCommandContext()` which
runs every command. Viper does file walks, env scans, type registrations.
Could be lazified (only init the keys that are actually read).

## Investigation history

For posterity — how we got here.

### Stripping iteration timeline

| Version | Patches added | bd show | bd ready | bd list | bd update |
|---|---|---:|---:|---:|---:|
| brew v1.0.2 | baseline | 1171 | 1207 | 1491 | 1909 |
| vanilla HEAD | none (#3691 fix only) | 2985 | 3130 | 2668 | 3760 |
| v1 (gascity-fast) | auto-backup, auto-export off | 916 | 1078 | 1229 | 2021 |
| v2 | + auto-import probe, multi-db, molecules | 542 | 795 | 777 | 1796 |
| v3 | + git subprocess, version tracking, identity | 566 | 756 | 726 | 1240 |
| v4 | + tips | 529 | 474 | 696 | 1021 |
| v5 | + `bd nuke` (new command, no perf impact) | 460 | 299 | 338 | 1051 |
| v6 | + hook re-fetch skip | 359 | 264 | 277 | 735 |
| **v7** | + auto-push hardcoded off + sync.remote hardcoded "" | **276** | **326** | **295** | **728** |

Note v7: bd show improved (-23% vs v6); ready/list within noise; update flat.
The auto-push/sync.remote strips are mostly **safety** patches — they prevent
accidental remote pushes if config is re-introduced — not perf-driven.

### Key findings

1. **Vanilla HEAD is 2× slower than v1.0.2** on our setup. Speed regression
   between releases comes from added `PersistentPreRun` checks
   (`auto-migrate`, more aggressive `auto-backup`/`auto-export` retries,
   doctor-style validations).

2. **`bd 1.0.2` does not actually trigger the `#3849` auto-import bug** on
   our setup. The "auto-importing N bytes into empty database" warning is
   absent because our `dolt.auto-commit: batch` config bypasses the affected
   code path. So the upstream fix (#3691) didn't speed us up — fix was
   moot for our trigger conditions.

3. **CPU profile shows 100% I/O wait.** All remaining bd time is waiting on
   dolt MySQL responses; no CPU work to optimize.

4. **`HookFiringStore.fireHookByID` was the biggest single offender** for
   writes: 1 extra `GetIssue` DB roundtrip per `UpdateIssue` to pass full
   Issue state to hook scripts. Removing it cut `bd update` from ~1000 ms
   to ~300 ms (warm cache).

5. **`sync.remote` defense-in-depth.** Stripping config alone isn't enough —
   `bd init` re-adds `sync.remote` from git remote automatically. Hardcoding
   `resolveSyncRemote()` to `""` and `maybeAutoPush()` to no-op makes the
   binary itself refuse to push regardless of config drift. Verified: with
   `sync.remote` re-introduced, `bd update` stays at ~450ms instead of
   blocking on git+ssh push.

### Tools used

- `/usr/bin/time -p` — real/user/sys breakdown
- `BD_OTEL_STDOUT=true` — OTel spans (bd command, dolt.query, hook.exec)
- `bd --profile <cmd>` → `go tool pprof -top -cum` — CPU profile
- `.runtime/bench/run.sh` — repeatable bench (N iterations of show/ready/list/update)

## Related

- Upstream issues that informed this work:
  [#1896](https://github.com/gastownhall/gascity/issues/1896) (gc status fan-out),
  [#2093](https://github.com/gastownhall/gascity/issues/2093) (bd update auto-import 9s),
  [#1978](https://github.com/gastownhall/gascity/issues/1978) (gc agents shell out),
  [beads#3849](https://github.com/gastownhall/beads/issues/3849) (root cause auto-import),
  [beads#3691](https://github.com/gastownhall/beads/pull/3691) (fix),
  [beads#3760](https://github.com/gastownhall/beads/issues/3760) (bd serve daemon proposal).
- Internal: [SUPERVISOR-RECOVERY.md](./SUPERVISOR-RECOVERY.md) — playbook for
  a stuck supervisor (often a downstream symptom of bd slowness).
- Internal: `scripts/bd-trace.sh` — per-call tracing harness.
- Raw bench data: `.runtime/bench/SUMMARY.md` and `.runtime/bench/*.txt`.
