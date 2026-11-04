# HANDOFF — OpenList ⇄ Emby work (continue in a fresh chat)

You are picking up work done in a previous chat. Everything needed to continue
without that chat's context is below. Work through it in order.

The repo is `yhchong2014/teldrive`. The artifacts live in `openlist-emby/` at the
repo root — they are committed alongside this file, so a fresh clone has them.

---

## 0. Background: what the previous session established

The user runs **Teldrive** (this repo) as storage and **OpenList** as the front
end, with **Emby-compatible clients** (Infuse, VidHub, etc.) connecting to
OpenList's `/@emby` endpoint. They asked a series of questions that turned into
real findings, then into two deliverables.

Established facts — do not re-derive these, they are verified against source:

- OpenList's Emby API is at `/@emby` (also `/@mediabrowser`); reported version
  `4.10.0.5`. Both `media_enabled` and `media_emby_enabled` settings default to
  **off** (endpoints 404 until enabled).
- Emby `DateCreated` comes from `media_items.created_at` — **when the scanner
  first indexed the file**, not the file's date and not its release date.
  `PremiereDate` comes from `media_items.release_date`.
- OpenList's `sortEmbyItems()` supports exactly: `SortName`, `Name`,
  `SeriesSortName`, `DateCreated`, `DatePlayed`, `ProductionYear`,
  `ParentIndexNumber`, `IndexNumber`, `CommunityRating`.
  **There is no `PremiereDate`/`ReleaseDate` sort key** — a client asking for it
  silently gets no sorting at all. This is why "Release Date" sorting is broken
  out of the box and why deliverable 1 works by rewriting `created_at`.
- `UpdateMediaItemReq` has **no date field**; the admin UI cannot edit date added.
- `SortName` is set to a verbatim copy of `Name` at all four DTO constructors.
  `ForcedSortName` is declared in the DTO but **never assigned anywhere**.
- Watch state (`played`, `play_count`, `is_favorite`, `position_ticks`) lives in
  `media_user_data`, per `(user_id, item_id, episode_index)`. It works, but:
  - `MediaItem.Duration` is **never assigned** anywhere in the codebase, so the
    `ratio >= 0.9` auto-mark-watched in `embySaveProgress`/`embySessionPlaying`
    never fires. Emby playback never auto-marks a movie watched.
  - `seriesItem()` hardcodes `UserData: &embyUserData{}` — no watched badge on
    series.
  - `DeleteMediaItem` / `ClearMediaItems` delete `media_annotations` but **not**
    `media_user_data` — orphaned watch state after a wipe.
- StrmAssistant (the Emby plugin) **cannot** be used with OpenList. It is a
  server-side plugin that injects `ILibraryManager`, `IProviderManager`,
  `IFileSystem` inside the Emby process; it never calls a remote Emby API.
  OpenList also has no plugin/hook system (the `plugin` branch is WASM *storage
  drivers* only).
- OpenList's Emby search is one line: case-insensitive `strings.Contains` on the
  display name only. No pinyin, no traditional/simplified, no fuzzy, no
  alternate titles (`OriginalTitle` is declared and never assigned; there is no
  alternate-title column at all).

---

## 1. What already exists in `openlist-emby/`

| file | status | what it is |
|---|---|---|
| `set_date_added.py` | **DONE, tested** | Rewrites `media_items.created_at` from `release_date`. Dry-run by default, `--apply` writes, auto `.bak`. sqlite default + `--backend mysql\|postgres`. |
| `pinyin-sort-v4.2.8.patch` | **WRITTEN, NOT COMPILED** | Adds 拼音首字母排序 to OpenList's Emby DTOs. Verified to apply cleanly to pristine `v4.2.8`. |
| `PINYIN-SORT-PATCH.md` | done | Full explanation + apply instructions for the patch. |
| `README.md` | done | Usage for `set_date_added.py`. |
| `pinyin_patch_validate.py` | done | Python port of the Go logic; 23 cases, all pass. |
| `pinyin_sort_demo.py` | done | Before/after ordering demo. |
| `chinese_search_reference.py` | **REFERENCE ONLY** | Working pinyin + t2s + fuzzy matcher, 16 cases pass. Not yet a Go patch. |

---

## 2. Your tasks, in priority order

### Task 1 — Compile and test the pinyin-sort patch (highest priority)

The previous session had **no Go toolchain and no root**, so the patch was never
built. This is the only thing blocking it.

```bash
git clone https://github.com/OpenListTeam/OpenList /tmp/OpenList
cd /tmp/OpenList && git checkout v4.2.8
git apply /path/to/openlist-emby/pinyin-sort-v4.2.8.patch
go get github.com/mozillazg/go-pinyin@latest && go mod tidy
go build ./...
go test ./server/handles/ -run 'Pinyin|SortName' -v
```

Expected: build clean, all `TestEmbySortName` / `TestPinyinSortName*` /
`TestPinyinSortOrder` pass.

If it does not compile, the likely causes in order:
1. `go-pinyin` API drift — the code uses `pinyin.NewArgs()`,
   `pinyin.LazyPinyin(s, args)`, `pinyin.FirstLetter`. Those are correct for
   v0.20.x. Check `pinyin.Args` still has `Style`.
2. `ForcedSortName` field name — confirm it exists in `embyItem` in `emby.go`.
3. gofmt alignment inside the struct literals (cosmetic; `gofmt -w` fixes it).

Also run `gofmt -l server/handles/` and fix any formatting, then regenerate the
patch so it is gofmt-clean.

### Task 2 — Port Chinese search to Go (if the user wants it)

`chinese_search_reference.py` is the validated spec. Port to Go as
`server/handles/search_zh.go`, and swap the two `strings.Contains` sites:

- `filterEmbyItems()` (used by `/Items`, `/Users/{id}/Items`, `embyChildren`,
  `embyLatest`)
- `embySearchHints()` (`/Search/Hints`)

Matching order that the tests require:
1. plain lowercase substring (current behaviour, keep it first)
2. traditional↔simplified normalisation, **both directions**
3. pinyin: full (`liulangdiqiu`) and initials (`lldq`), **strip spaces from the
   query** so `liu lang` matches — this was a real bug caught in testing
4. must NOT match: wrong initials (`ldlsw`), wrong polyphone reading (`zqsl`)

Add `github.com/longbridgeapp/opencc` for t2s. Reuse `pinyinPhrases` from
`pinyin.go`. Keep the same 16-case table as a Go test.

### Task 3 — Open question the user never answered

Before the chat ended the user was asked whether pinyin sort should also apply
to OpenList's **own web UI file browser** (not just Emby). They replied "as long
as it can be done on openlist and emby client can accept it" — i.e. they care
about the Emby path, and did not commit to the web UI. Confirm before doing it.

If they do want it: `model.SortFiles` uses `github.com/maruel/natural`, whose
`commonPrefix()` compares **bytes** (`ca := a[i]`), so it has the identical CJK
bug. Needs a pinyin-aware comparator gated on `hasHan`.

### Task 4 — Optional: the unfinished watch-state fixes

Discovered but never fixed (user never asked for them; offer, don't assume):

- Populate `MediaItem.Duration` during scan (ffprobe, or TMDB `runtime`), so the
  90% auto-mark-watched actually fires.
- Aggregate `UserData` for `seriesItem()` instead of hardcoding `&embyUserData{}`.
- Cascade-delete `media_user_data` in `DeleteMediaItem` / `ClearMediaItems` /
  `DeleteMediaItemsByIDs`.

---

## 3. Hard-won details — do not lose these

**Timestamp format for sqlite.** OpenList's driver is `glebarez/sqlite`, which
parses with `parseTimeFormats[0] = "2006-01-02 15:04:05.999999999-07:00"` and
writes with the same. `set_date_added.py` emits exactly
`"%Y-%m-%d %H:%M:%S+00:00"` to match. Do not change it to ISO `T` form.

**Store noon, not midnight.** `embyTimeStr()` formats with `.UTC()`, so a
midnight timestamp can slip back one calendar day for negative UTC offsets.
Noon can never cross a day boundary anywhere. `set_date_added.py --time`
defaults to `12:00:00` for this reason.

**Never touch `updated_at`.** OpenList uses it for Subsonic's `Starred` field.
Also note MySQL evaluates `SET` clauses left-to-right, so
`SET created_at=?, updated_at=created_at` would copy the *new* value — the MySQL
backend deliberately only sets `created_at`.

**`go-pinyin` drops unknown runes.** `Pinyin()` only appends a rune if it has a
dictionary entry, so `流浪地球2` would lose the `2`. `pinyin.go` only converts
Han characters and appends the rest verbatim.

**Polyphones are the actual risk.** The single-char table puts the most common
reading first: `重庆` → `ZQ` (zhòng, wrong), `长城` → `ZC` (zhǎng, wrong),
`乐器` → `LQ` (lè, wrong). `pinyinPhrases` in `pinyin.go` overrides ~25
high-frequency film/music words, **longest-match-first** so `重庆森林` is not
split as `重庆`+`森林`.

**TV episodes have no release date.** `PremiereDate` is emitted only for movies
(`movieItem`). Episodes inherit the single merged show record's `DateCreated`.
Running `set_date_added.py` on TV stamps every episode with the show's release
date — harmless, in-series order comes from `ParentIndexNumber`/`IndexNumber`,
but say so before the user runs it.

**Mixed EN/CN sorting.** Without the patch, every ASCII title sorts before every
CJK one (ASCII codepoints are lower), so all Chinese films land after all
English ones. That is the symptom users notice most.

**The user's DB.** sqlite by default at `<data-dir>/data.db`, table prefix `x_`
(configurable). Docker data volume is `/opt/openlist/data/`. If they run MySQL,
`set_date_added.py --backend mysql --dsn ...` exists.

---

## 4. Working rules

- The user's fork is this repo (`yhchong2014/teldrive`). Artifacts belong in
  `openlist-emby/`. Do not touch `docs/` — that is an Astro site.
- Do not modify Teldrive's Go code for any of this; these are OpenList-side
  changes plus standalone scripts.
- Where the code supports it, **always test rather than assert**. The previous
  session's Python ports caught three real bugs (`liu lang` not matching,
  `乐器` reading, and a wrong test expectation for `十二` = S+E).
- Be explicit about verification status. Never describe the patch as "working"
  until `go build` + `go test` have actually run.
- If the user asks "does X work in OpenList", check the source before answering.
  Several intuitions were wrong (e.g. pinyin sort looked like it should work via
  `ForcedSortName`, but OpenList never sets it).

---

## 5. Quick verification commands

```bash
# date script still sane
python3 openlist-emby/set_date_added.py --help
python3 openlist-emby/pinyin_patch_validate.py    # 23 cases, ALL PASS
python3 openlist-emby/pinyin_sort_demo.py         # before/after ordering
python3 openlist-emby/chinese_search_reference.py # 16 cases, ALL PASS
# requires: pip install pypinyin

# patch applies to pristine v4.2.8
git clone --depth 1 -b v4.2.8 https://github.com/OpenListTeam/OpenList /tmp/v
cd /tmp/v && git apply --check /path/to/openlist-emby/pinyin-sort-v4.2.8.patch
```
