# Local Forward Tracking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let each source choose how already-forwarded mails are remembered: the existing `$Forwarded` IMAP flag (default) or a local bbolt database, so servers that drop custom keywords on disconnect (poczta.onet.pl) stop re-forwarding everything after each restart.

**Architecture:** A new `StateStore` (bbolt) records forwarded UIDs per `trackingId` + folder, together with the folder's `UIDVALIDITY`. `Forwarder` picks its behaviour from `SourceConfig.LocalTracking`: when false (default) nothing changes; when true it searches `NOT SEEN`, drops UIDs found in the store, records each UID right after a successful `Send`, and never writes flags to the source mailbox.

**Tech Stack:** Go 1.22, `go.etcd.io/bbolt` (pure Go, new dependency), `go-imap/v2` beta.8, Docker (alpine), Angular website.

**Spec:** No separate spec file. The design was approved in conversation on 2026-10-26; its requirements are reproduced in Global Constraints.

## Global Constraints

- Per-source boolean `localTracking` (default `false`, behaviour exactly as today with the `$Forwarded` flag). `localTracking: true` together with `deleteAfterForward: true` is a config validation error (a deleted mail needs no memory).
- Per-source `trackingId` (string): required (non-empty) when `localTracking` is true, must be unique among `localTracking` sources, ignored otherwise.
- Global `statePath` (string): default `state.db` in the directory of the config file; a relative value is resolved against that directory too. The database is opened only if at least one source has `localTracking: true`; failure to open it aborts startup with exit code 1.
- Store key is `trackingId` + folder (never the source `name`, which is not validated as unique).
- `UIDVALIDITY` stored per `trackingId` + folder; when it changes, that folder's recorded UIDs are cleared and a warning is logged.
- With `localTracking` the source mailbox is never modified (no `STORE $Forwarded`).
- Search stays `NOT SEEN`; with `localTracking` UIDs present in the store are removed from the result.
- A UID is recorded immediately after each successful `Send`, one mail at a time. A failed `Send` records nothing.
- First start with an empty database forwards everything unseen (the user's existing unseen mails have not been forwarded yet).
- Go 1.22 (`go.mod` stays `go 1.22`), strict error handling, no ignored errors, minimal dependencies.
- Per `AGENTS.md` the task is done only when `just check`, `just docker-build`, README, `config.example.json`, website config editor (`website/src/app/components/config-tool.ts`) and setup guide (`website/src/app/components/setup.ts`) are updated and `cd website && npx ng build` succeeds.
- **Running Go tooling:** build, vet and test in a container: `docker run --rm -v "$PWD":/app -w /app golang:1.22-alpine <cmd>` (referred to below as `GO <cmd>`), and lint with the `golangci/golangci-lint` image. Where a native `go` or `just` is available, the equivalent `just` recipes may be used instead.

## Review Focus

- `UIDVALIDITY` changes between runs: folder state is cleared and mails are re-sent (Task 2 test).
- Two `localTracking` sources sharing a `trackingId`, or one without it: config error naming the source index; `localTracking` combined with `deleteAfterForward`: config error (Task 1 tests).
- Existing configs without any new field still validate and keep the `$Forwarded` flag behaviour (Task 1 test).
- `statePath` pointing into a missing or unwritable directory: startup error, not a panic or silent fallback (Task 2 test).
- Several folders of one source writing to the store concurrently (Task 2 test).
- Docker: `appuser` (uid 1001) cannot write to `/app` (created by root in the `Dockerfile`), so the documented default would fail in the container; a writable `/data` volume is required (Task 4).
- A mail the user reads in the mailbox before it is forwarded is skipped (existing `NOT SEEN` behaviour, kept; documented in README).

---

### Task 1: Config fields and validation

**Files:**
- Modify: `src/config.go` (`SourceConfig`, `Config`, `validateConfig`, `loadConfig`)
- Test: `src/config_test.go`

**Interfaces:**
- Produces: `SourceConfig.LocalTracking bool` (`json:"localTracking,omitempty"`), `SourceConfig.TrackingID string` (`json:"trackingId,omitempty"`), `Config.StatePath string` (`json:"statePath,omitempty"`); `func (c *Config) usesLocalTracking() bool` (true if any source has `LocalTracking`).
- After `loadConfig`, `Config.StatePath` is always non-empty and absolute-or-config-dir-joined; `loadConfig` does not touch `LocalTracking`.

- [ ] **Step 1: Write failing tests** in `src/config_test.go`, following the file's existing table/helper style:
  - `TestValidateConfig_LocalTrackingDefaultsToFalse`: source without the field validates, `LocalTracking == false`, `usesLocalTracking()` is false.
  - `TestValidateConfig_LocalTrackingWithDelete`: `localTracking` + `deleteAfterForward` → error containing `sources[0].localTracking` and `deleteAfterForward`.
  - `TestValidateConfig_LocalRequiresTrackingID`: `localTracking` with empty id → error containing `sources[0].trackingId`.
  - `TestValidateConfig_LocalDuplicateTrackingID`: two `localTracking` sources with the same id → error containing `sources[1].trackingId` and `sources[0]`.
  - `TestValidateConfig_FlagIgnoresTrackingID`: a non-local source never clashes with a local one, and two non-local sources with the same id are fine.
  - `TestLoadConfig_StatePathDefault`: config file in a temp dir without `statePath` → `cfg.StatePath == filepath.Join(dir, "state.db")`; with `"statePath": "sub/x.db"` → `filepath.Join(dir, "sub/x.db")`; with an absolute path → unchanged.
- [ ] **Step 2: Run** `GO go test -count=1 -run 'LocalTracking|TrackingID|StatePath' ./src/` → FAIL (undefined fields).
- [ ] **Step 3: Implement** in `src/config.go`: the fields and constants above; in `validateConfig`'s source loop reject `LocalTracking` combined with `DeleteAfterForward`, require `TrackingID` when `LocalTracking`, track seen ids in a `map[string]int` (id → source index) for the duplicate error; in `loadConfig`, after validation, resolve `StatePath` against `filepath.Dir(path)`; add `usesLocalTracking`.
- [ ] **Step 4: Run** `GO go test -count=1 ./src/` → PASS (existing config tests included).
- [ ] **Step 5: Commit** `feat: add localTracking, trackingId and statePath config`.

### Task 2: StateStore (bbolt)

**Files:**
- Create: `src/state.go`, `src/state_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces:
  - `func OpenStateStore(path string) (*StateStore, error)` — creates the file if missing, opens with a 5 s lock timeout (`bolt.Options{Timeout: 5 * time.Second}`) so a second instance fails instead of hanging; error wraps the path.
  - `func (s *StateStore) Close() error`
  - `func (s *StateStore) SyncUIDValidity(trackingID, folder string, validity uint32) (reset bool, err error)` — nothing stored yet: store it, `reset=false`; equal: no-op; different: delete all UIDs of that folder, store the new value, `reset=true`.
  - `func (s *StateStore) FilterUnforwarded(trackingID, folder string, uids []imap.UID) ([]imap.UID, error)` — returns the UIDs not recorded, original order preserved; never returns an error for unknown id/folder (everything is unforwarded).
  - `func (s *StateStore) MarkForwarded(trackingID, folder string, uid imap.UID) error` — one write transaction; idempotent.
- Layout (decided here): root bucket `sources` → bucket `<trackingID>` → bucket `<folder>` holding key `uidvalidity` (4-byte big-endian) and sub-bucket `uids` (key: 4-byte big-endian UID, value: UTC RFC3339 time).

- [ ] **Step 1: Add the dependency:** `GO go get go.etcd.io/bbolt@v1.3.11` (expected to keep `go 1.22` in `go.mod`; if it raises the Go version, use the newest v1.3.x that does not) and confirm `go.mod` still says `go 1.22`.
- [ ] **Step 2: Write failing tests** in `src/state_test.go` using `t.TempDir()`:
  - `TestStateStore_MarkAndFilter`: mark 2 and 4; `FilterUnforwarded(..., {1,2,3,4,5})` returns `{1,3,5}`.
  - `TestStateStore_PersistsAcrossReopen`: mark, `Close`, reopen the same path, filter shows the UID still recorded.
  - `TestStateStore_IsolatedByTrackingIDAndFolder`: a UID marked under id `a`/`INBOX` is unforwarded under id `b`/`INBOX` and `a`/`Other`.
  - `TestStateStore_SyncUIDValidity`: first call → `reset=false`; same value again → `false` and marks kept; new value → `reset=true` and marks gone; other folders untouched.
  - `TestStateStore_MarkIdempotent`: marking the same UID twice returns nil and does not duplicate.
  - `TestStateStore_ConcurrentMark`: 8 goroutines mark 100 distinct UIDs each in two folders; afterwards `FilterUnforwarded` of all 800/folder UIDs returns empty.
  - `TestOpenStateStore_BadPath`: path inside a non-existent directory → non-nil error mentioning the path.
- [ ] **Step 3: Run** `GO go test -count=1 -run 'StateStore' ./src/` → FAIL (undefined).
- [ ] **Step 4: Implement** `src/state.go` to the signatures above.
- [ ] **Step 5: Run** `GO go test -count=1 ./src/` → PASS; `GO go vet ./src/` clean.
- [ ] **Step 6: Commit** `feat: add bbolt state store for forwarded UIDs`.

### Task 3: Forwarder and wiring

**Files:**
- Modify: `src/forwarder.go`, `src/manager.go`, `src/main.go`
- Modify (mechanical call-site updates): `src/forwarder_test.go`, `src/health_test.go`
- Test: `src/forwarder_test.go`

**Interfaces:**
- Consumes: Task 1 config fields/constants, Task 2 `StateStore` methods.
- Produces:
  - `func NewForwarder(source SourceConfig, sender Sender, state *StateStore, onStatusChange func(ForwarderStatus)) *Forwarder` (`state` may be nil when no source uses `localTracking`).
  - `func NewManager(config *Config, state *StateStore) *Manager`.
  - `func searchCriteria(localTracking bool) *imap.SearchCriteria` — false: `NotFlag{\Seen, $Forwarded}` (identical to today); true: `NotFlag{\Seen}` only.
  - `forwardNewMessages(ctx, client, folder string)` and `forwardMessage(ctx, client, folder string, uid imap.UID)` gain a `folder` argument; their callers (`monitorFolder`, `watchFolder`, `pollFolder`) pass it through.

- [ ] **Step 1: Write failing tests** in `src/forwarder_test.go`:
  - `TestSearchCriteria_FlagTracking`: `NotFlag` contains both `\Seen` and `$Forwarded`.
  - `TestSearchCriteria_LocalTracking`: `NotFlag` equals exactly `{\Seen}`.
  - `TestForwarder_LocalTrackingRequiresState`: `NewForwarder` for a `localTracking` source with `state == nil` is a programming error — assert that `Run` returns promptly and `GetStatus().Error` is non-empty, rather than panicking (implementer: pick the simplest check that cannot nil-deref; `main` guarantees non-nil, this is a safety net).
- [ ] **Step 2: Update existing call sites** to the new signatures (pass `nil` for `state`) in `forwarder_test.go` (4 calls) and `health_test.go` (5 `NewManager`, 4 `NewForwarder`); run `GO go vet ./src/` to find any left.
- [ ] **Step 3: Run** the new tests → FAIL, then **implement** in `src/forwarder.go`:
  - `Forwarder` gets a `state *StateStore` field.
  - `monitorFolder`: keep `SelectData` from `client.Select(...)`; when `LocalTracking` call `state.SyncUIDValidity(source.TrackingID, folder, selectData.UIDValidity)` (verify the field name with `go doc github.com/emersion/go-imap/v2.SelectData`) and log a warning with the folder when `reset` is true; return the error otherwise wrapped.
  - `forwardNewMessages`: use `searchCriteria(f.source.LocalTracking)`; when `LocalTracking` pass `AllUIDs()` through `state.FilterUnforwarded`; the "Found N new message(s)" log reports the filtered count.
  - `forwardMessage`: after a successful `Send`, when `LocalTracking` call `state.MarkForwarded(...)`; a failure here is logged as an error (message will be retried and may duplicate once) and does not stop the loop; skip the `STORE $Forwarded` block entirely when `LocalTracking`. The `deleteAfterForward` block is unchanged (the two options are mutually exclusive by validation).
- [ ] **Step 4: Wire** `src/manager.go` (store the `*StateStore`, pass to `NewForwarder`) and `src/main.go`: after `loadConfig`, `if config.usesLocalTracking() { open store at config.StatePath; on error log it and exit 1; defer Close }`; ensure the store is closed after `manager.StartAll` returns (before process exit, not via `os.Exit` skipping defers).
- [ ] **Step 5: Run** `GO go build ./src/ && GO go vet ./src/ && GO go test -count=1 ./src/` → all PASS.
- [ ] **Step 6: Commit** `feat: forward tracking via local state store`.

### Task 4: Docker, docs and website

**Files:**
- Modify: `Dockerfile`, `README.md`, `config.example.json`, `website/src/app/components/config-tool.ts`, `website/src/app/components/setup.ts`

**Interfaces:** consumes the final config field names from Task 1.

- [ ] **Step 1: Dockerfile.** In the production stage create `/data` owned by `appuser` (`mkdir /data && chown appuser:appgroup /data`, before `USER appuser`) and declare `VOLUME /data`. Reason: `/app` is created by root, so uid 1001 cannot create `/app/state.db`.
- [ ] **Step 2: `config.example.json`.** Add `"statePath": "/data/state.db"` at top level and, on the first source (`deleteAfterForward: false`), `"localTracking": true`, `"trackingId": "work"`; the second source (`deleteAfterForward: true`) stays unchanged.
- [ ] **Step 3: README.** Add `localTracking`, `trackingId`, `statePath` rows to the configuration reference; a "Tracking forwarded messages" section explaining when to use `localTracking` (servers that do not keep custom flags across connections, e.g. poczta.onet.pl), the first-start behaviour (forwards every unseen mail), that it cannot be combined with `deleteAfterForward`, that changing `trackingId` or toggling `localTracking` starts from empty memory, that reading a mail before it is forwarded makes it skipped, and the single possible duplicate after a crash between send and record; add the `/data` volume to the `docker run` and Compose examples (note a bind mount must be writable by uid 1001); update the "How It Works" list (step 4 depends on `localTracking`).
- [ ] **Step 4: Website config editor** (`config-tool.ts`): per-source `localTracking` checkbox (disabled while `deleteAfterForward` is checked, and vice versa) and a required `trackingId` text field shown only when it is checked; global `statePath` field shown when any source has it checked; emit them in `configJson` only when non-default (`localTracking`/`trackingId` only when checked, `statePath` only when set and any source uses it); add the new controls in `createSource()` and the form group. Follow the existing `targetFolder` / `deleteAfterForward` pattern.
- [ ] **Step 5: Website setup guide** (`setup.ts`): add the `/data` volume to the Docker and Compose snippets and a short note on `localTracking`.
- [ ] **Step 6: Verify** `cd website && npx ng build` succeeds; `docker build -t imapforward .` succeeds (equivalent of `just docker-build`).
- [ ] **Step 7: Commit** `docs: document local tracking and add /data volume`.

### Task 5: Full verification

- [ ] **Step 1:** `GO sh -c 'go build -o /tmp/imapforward ./src/ && go vet ./src/ && go test -count=1 ./src/'` → PASS; run the linter (`just lint`, or the `golangci/golangci-lint` image against `./src/`) → no findings; `gofmt -l src/` prints nothing.
- [ ] **Step 2:** `docker build -t imapforward .` and `cd website && npx ng build` → success.
- [ ] **Step 3: Container smoke test** (no mail is sent): run the image with a deliberately unreachable source with `localTracking: true` and `-v imapforward-data:/data`; expect startup log without a state-open error and `/data/state.db` created; run again with `statePath` pointing to `/app/x.db` and expect a clear startup error and exit code 1.
- [ ] **Step 4: Hand over to the user** the real-account check only they can do (needs their credentials and actually forwards mail): start with `localTracking: true` on the Onet source, stop the container after a few mails, start it again, and confirm already forwarded mails are not sent again.
