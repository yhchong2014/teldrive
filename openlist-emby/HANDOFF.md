# HANDOFF — OpenList ⇄ Emby work (instructions only)

You are picking up work done in a previous chat. This document is
**instructions only — there is deliberately no code in this repo.** Build the
code yourself following the specs below.

**Owner's constraint: do not commit code, patches, or scripts to this fork
(`yhchong2014/teldrive`).** If you produce code, keep it in `/tmp` or a scratch
directory, or deliver it in chat. Only this document lives here.

Context: the owner uses **Teldrive** (this repo) as storage, **OpenList** as the
front end, and **Emby-compatible clients** (Infuse, VidHub, MoviePilot) pointed at
OpenList's `/@emby` endpoint. They want OpenList's Emby surface to behave more
like a real Emby — specifically **date added**, **拼音首字母排序**, and later
**Chinese search**.

---

## 1. Verified facts — do NOT re-derive these

These were checked against OpenList source (v4.2.8 and `main`). Trust them and
spend your time on the work instead.

### Emby API surface
- Endpoint: `/@emby` (also `/@mediabrowser`), reported version `4.10.0.5`.
- Gated by two settings, **both default off**: `media_enabled` and
  `media_emby_enabled`. Endpoints return 404 while off.
- Auth: `POST /Users/AuthenticateByName`, or an app key used as `api_key` /
  `X-Emby-Token`.
- Playback: `PlaybackInfo` → `302` to a signed link.
  `SupportsTranscoding: false` — **no transcoding**.
- Missing/unimplemented: no subtitle streams (only lyrics-as-subtitle for songs),
  `DateLastMediaAdded` declared but never assigned, no `UserDataChanged` push
  (WebSocket is keep-alive only).

### Date added
- Emby `DateCreated` ← `media_items.created_at` — **when the scanner first
  indexed the file**. Not the file mtime, not the release date.
- `PremiereDate` ← `media_items.release_date`.
- `UpdateMediaItemReq` has **no date field**. The admin UI cannot edit it.
- The scanner never reads `ModTime()` — `buildMediaItemFromVFS` records only
  name, size, MIME.
- Rescans **preserve** `created_at` (`item.CreatedAt = existing.CreatedAt`), so
  edits survive rescans. Hard delete + rescan assigns a fresh timestamp.

### Sorting — the critical finding
`sortEmbyItems()` supports exactly these keys:

```
SortName · Name · SeriesSortName · DateCreated · DatePlayed
ProductionYear · ParentIndexNumber · IndexNumber · CommunityRating
```

**There is no `PremiereDate` / `ReleaseDate` key.** A client sending
`SortBy=PremiereDate` matches no case, `cmp()` returns 0 for every pair, and the
sort becomes a no-op — items come back in assembly order, which looks random.
This is why "Release Date" sorting appears broken, and why the date fix works by
moving the release date **into** `created_at`.

Also: at all four DTO constructors (`movieItem`, `seriesItem`, `albumItem`,
`songItem`) `SortName` is a **verbatim copy of `Name`**. `ForcedSortName` is
declared in the DTO but **never assigned anywhere** in the codebase — a free,
unused hook.

### Watch state
Stored in `media_user_data`, keyed `(user_id, item_id, episode_index)` with
`played`, `play_count`, `is_favorite`, `position_ticks`, `last_played_at`.
Works, but:
- `MediaItem.Duration` is **never assigned** anywhere ⇒ the
  `ratio >= 0.9` auto-mark-watched never fires. Emby playback never auto-marks a
  movie watched; only manual "mark played" increments `PlayCount`.
- `seriesItem()` hardcodes `UserData: &embyUserData{}` ⇒ no watched badge on
  series (seasons get `UnplayedItemCount`, episodes get `Played`).
- `DeleteMediaItem` / `ClearMediaItems` / `DeleteMediaItemsByIDs` delete
  `media_annotations` but **not** `media_user_data` ⇒ orphaned watch state.
- `ClearMediaUserDataByUser` exists in the DB layer but is never called.

### Search
- One line, case-insensitive `strings.Contains` on the **display name only**.
  Two call sites: `filterEmbyItems()` and `embySearchHints()`.
- No pinyin, no traditional↔simplified, no fuzzy, no alternate titles.
  `OriginalTitle` is declared and never assigned; there is **no**
  alternate-title column at all. The TMDB scraper doesn't even fetch
  `original_title`.
- OpenList's own Bleve index sets no CJK analyzer either.

### StrmAssistant cannot be used
`sjtuross/StrmAssistant` is a **server-side** Emby plugin. Its constructor
injects `ILibraryManager`, `IProviderManager`, `IFileSystem`, `ITaskManager` and
runs **in-process**; it never calls a remote Emby API, so there is nothing to
point at OpenList. It also targets `mediabrowser.server.core` 4.8.0.80,
`netstandard2.1`, min Emby 4.8.5. Its `main` is a "Lite" build
(`AssemblyName` = `StrmAssistantLite`) that ships the resource strings for the
Chinese options but not the code. OpenList additionally has **no plugin/hook
system** — the `plugin` branch is WASM **storage drivers** only (13 ahead /
401 behind `main`). Conclusion: any Chinese-search or pinyin feature must be
implemented **inside OpenList**.

---

## 2. Deliverable A — Date added = release date

**Goal:** make Emby clients sort movies by true release date.

**Method:** rewrite `media_items.created_at` from `media_items.release_date` in
OpenList's own database. This is a local DB edit, not a feature — there is no UI
or API field, so direct SQL is the only route.

### Build this as a script (keep it out of the repo)

Requirements:
- Default to **dry run**; require an explicit `--apply` to write.
- On `--apply`, copy a timestamped `.bak` of the sqlite file first.
- Default target: sqlite at `<data-dir>/data.db`, table prefix `x_`
  (both configurable). Docker data volume is `/opt/openlist/data/`.
- Also support MySQL and Postgres via `--backend` + `--dsn`.
- Filter: `deleted_at IS NULL`; skip hidden unless asked; default media type
  `video`, allow `music` / `image` / `book` / `all`.
- Skip rows with no `release_date`, list them at the end, and offer a
  `--fallback YYYY-MM-DD` for them.
- **Idempotent** — re-runs must report "already correct" rather than rewriting.

### Non-obvious constraints — get these exactly right

1. **Timestamp format.** OpenList's sqlite driver is `glebarez/sqlite`. It parses
   with `parseTimeFormats[0] = "2006-01-02 15:04:05.999999999-07:00"` and writes
   time.Time with that same layout. Write the value as
   `YYYY-MM-DD HH:MM:SS+00:00` (e.g. `2009-11-13 12:00:00+00:00`). Do **not**
   use the ISO `T` form.
2. **Store noon UTC, not midnight.** `embyTimeStr()` formats with `.UTC()`, so a
   midnight timestamp can slip back one calendar day for negative UTC offsets.
   Noon can never cross a day boundary in any timezone (−12…+14).
3. **Never touch `updated_at`.** OpenList uses it for Subsonic's `Starred` field.
   Also, MySQL evaluates `SET` clauses left-to-right, so
   `SET created_at=?, updated_at=created_at` would copy the *new* value — if you
   ever do set both, MySQL needs `updated_at` assigned first.
4. Normalise input dates: accept `YYYY-MM-DD`, `YYYY/MM/DD`,
   `YYYY-MM-DDTHH:MM:SSZ`, and a bare 4-digit year.

### Usage shape to document for the owner

```
stop OpenList → dry run → review → --apply → start OpenList
→ POST /Library/Refresh (or client "Refresh metadata")
→ sort by "Release Date" in the client
```

### Tell the owner these three caveats

- **TV episodes have no release date of their own.** `PremiereDate` is emitted
  only by `movieItem()`. Episodes inherit the single merged show record's
  `DateCreated`, so running this on a TV library stamps every episode with the
  show's date. Harmless — in-series order comes from
  `ParentIndexNumber`/`IndexNumber` — but they should know.
- **Date Added and Release Date become identical**, so "Recently Added" stops
  meaning anything. It's one or the other, because `DateCreated` is the only
  date OpenList exposes for client-side sorting.
- Survives normal rescans; **not** a full library wipe + rescan.

---

## 3. Deliverable B — 拼音首字母排序

**Goal:** Emby clients sort Chinese titles by pinyin instead of Unicode codepoint.
**No client change should be needed** — clients already sort by `SortName`.

### The bug

`SortName = Name` verbatim at all four DTO sites; `sortEmbyItems` compares them
as plain strings. CJK codepoints are ordered radical-and-stroke, so Chinese
libraries come out in a meaningless order. Mixed libraries are worse: ASCII
codepoints are lower, so **every Chinese title sorts after every English one**.
That symptom is what users notice most:

```
before                    after
Alien                     Alien      (ALIEN)
Dune                      Dune       (DUNE)
Interstellar              Interstellar
战狼                      霸王别姬     (BWBJ)
流浪地球                   重庆森林     (CQSL)
霸王别姬                   流浪地球     (LLDQ)
                          战狼        (ZL)
```

### The change

1. **New file** in `server/handles/` (suggest `pinyin.go`) exposing one helper,
   e.g. `pinyinSortName(name string) string`.
2. **Patch 4 sites** in `server/handles/emby_browse.go` — `movieItem`,
   `seriesItem`, `albumItem`, `songItem` — setting **both**
   `SortName: pinyinSortName(name)` and `ForcedSortName: pinyinSortName(name)`.
   Setting `ForcedSortName` matters: Emby ranks it above `SortName`, and
   OpenList never sends it.
3. **Dependency:** `github.com/mozillazg/go-pinyin`.
4. **Tests:** a table test in `pinyin_test.go` (cases in §3.3).

### 3.1 Behaviour spec

- Pure-ASCII / digit-only titles → **return unchanged**. No uppercasing, no
  rewriting, and skip the cache. Keeps the patch a no-op for non-Chinese
  libraries.
- Titles containing Han characters → convert the **Han characters only** to
  pinyin initials, uppercase. Non-Han characters are preserved as-is.
- Mixed titles keep the Latin prefix and file under it:
  `Dune 沙丘` → `DUNE SQ`, not `SQ`.
- Episodes and seasons keep numeric `SortName` (`S01E02`) — they must stay in
  episode order, not alphabetical. Do not touch those constructors.
- Cache per title in a `sync.Map`. `loadEmbyLibrary` rebuilds the whole library
  on every request and the same series/album names recur across DTOs.

### 3.2 Two traps that will bite you

**`go-pinyin` silently drops unknown runes.** `Pinyin()` only appends a rune when
it has a dictionary entry, so `流浪地球2` loses the `2`. Convert only Han
characters yourself and append everything else verbatim.

**Polyphones are the real risk.** The single-character table puts the most
common reading first, which is wrong for many titles:

| word | naive result | correct |
|---|---|---|
| 重庆 | `ZQ` (zhòng) | `CQ` (chóng) |
| 长城 | `ZC` (zhǎng) | `CC` (cháng) |
| 乐器 | `LQ` (lè) | `YQ` (yuè) |
| 银行 | `XH` (xíng) | `YH` (háng) |

Fix with a curated phrase map, applied **longest-match-first** so `重庆森林` is
not split as `重庆`+`森林`. Cover at least:
重庆, 重庆森林, 长城, 长安, 长空, 长江, 长沙, 长春, 长大, 银行, 行长, 音乐,
乐器, 乐谱, 乐章, 乐队, 快乐, 重新, 重复, 重量, 重要, 行为, 行业, 同行, 人参,
曾经, 姓曾. Adding a phrase should be a one-line change.

API note: use `pinyin.NewArgs()`, `pinyin.LazyPinyin(s, args)`,
`pinyin.FirstLetter`. `LazyPinyin` force-sets `Heteronym=false`, so it does not
panic on multi-reading characters; `Pinyin()`/`SinglePinyin()` with
`Heteronym=true` can.

### 3.3 Required test cases

SortName expectations:

```
流浪地球→LLDQ   霸王别姬→BWBJ   杀手→SS      让子弹飞→RZDF
我不是药神→WBSYS 大话西游→DHXY   无间道→WJD   唐人街探案→TRJTA
战狼→ZL        长安十二时辰→CASESC  音乐之声→YYZS
重庆森林→CQSL (not ZQSL)   重庆→CQ (not ZQ)   长城→CC (not ZC)
长安→CA   银行→YH   乐器→YQ
Alien→Alien   Dune→Dune   Interstellar→Interstellar   ""→""
2001 A Space Odyssey→2001 A Space Odyssey  (must stay untouched)
Dune 沙丘→DUNE SQ
```

Plus: ASCII input must not be inserted into the cache; cache hits must be stable;
a mixed library must interleave (`Alien, 霸王别姬, 重庆森林, Dune, 流浪地球, 战狼`).

### 3.4 Verification gate

**Do not report this as working until it actually builds and the tests pass.**

```
git clone https://github.com/OpenListTeam/OpenList
git checkout v4.2.8
<apply your change>
go get github.com/mozillazg/go-pinyin@latest && go mod tidy
go build ./...
go test ./server/handles/ -run 'Pinyin|SortName' -v
```

If no Go toolchain is available (the previous session had none and no root to
install one), **say so explicitly** and hand over the diff with its verified
status. Do not describe an uncompiled patch as working.

Also run `gofmt -l server/handles/` and fix formatting before delivering.

For OpenList `main` instead of v4.2.8: `seriesItem`/`albumItem` context has
drifted slightly; `git apply --3way` merges it.

---

## 4. Deliverable C — 中文搜索增强 (only if the owner asks)

Not started beyond a validated reference design. Port to Go as
`server/handles/search_zh.go` and replace the two `strings.Contains` sites
(`filterEmbyItems`, `embySearchHints`).

Matching order, in this sequence:

1. plain lowercase substring — **keep first**, it's current behaviour
2. traditional↔simplified normalisation, **both directions**
   (`繁體` must match `繁体` and vice versa) — use `github.com/longbridgeapp/opencc`
3. pinyin: full (`liulangdiqiu`) and initials (`lldq`)
4. **strip spaces from the query** before comparing — `liu lang` must match
   `liulangdiqiu`. This was a real bug found in testing.

Reuse the phrase map from `pinyin.go`.

Must match: `流浪地球`✓, `liulang`✓, `lldq`✓, `流浪`✓, `liu lang`✓,
`cqsl`→重庆森林✓, `繁体`→繁體✓, `繁體`→繁体✓, `殺手`→杀手✓.
Must **not** match: `ldlsw`→流浪地球, `zqsl`→重庆森林, `zxcvbn`→流浪地球.

Scope decision to confirm with the owner: StrmAssistant defaults its search
scope to `Movie, Collection, Series`. OpenList has no Collection equivalent
(`/Collections` returns an empty list), so decide whether music/albums are
included.

---

## 5. Open questions — ask, don't assume

1. **Web UI sorting.** Should pinyin sort also cover OpenList's own web file
   browser, not just Emby? The owner said "as long as it can be done on openlist
   and emby client can accept it", which is ambiguous — it could mean "Emby path
   is enough" or "the whole of OpenList". **Confirm before doing it.** If yes:
   `model.SortFiles` uses `github.com/maruel/natural`, whose `commonPrefix()`
   compares **bytes** (`ca := a[i]`), so it has the identical CJK bug and needs a
   pinyin-aware comparator gated on `hasHan`.
2. **Search scope** — movies+series only, or music too? (§4)
3. **Watch-state fixes** — never requested, only discovered. Offer, don't
   implement unsolicited: populate `Duration` at scan time (ffprobe, or TMDB
   `runtime`), aggregate series-level `UserData`, cascade-delete
   `media_user_data`.
4. **Upstream or local?** No pinyin/中文搜索 issue exists on OpenList (checked),
   so there's no maintainer momentum to ride. Ask whether they want a PR to
   `OpenListTeam/OpenList` or just a local build.

---

## 6. Working rules

- **No code in this repo.** Deliver code in chat or keep it in `/tmp`.
- Never modify Teldrive's Go code for any of this — these are OpenList-side
  changes plus a standalone DB script.
- **Test rather than assert.** The previous session's reference ports caught
  three real bugs (`liu lang` not matching; `乐器` reading as lè not yuè; a wrong
  test expectation where `十二` = S+E, not just `十二`→`SE`). Expect the same
  class of bug in a Go port.
- Verify claims about OpenList against source before answering. Several
  intuitions were wrong, e.g. pinyin sort looked achievable via `ForcedSortName`
  alone, but OpenList never sets that field at all.
- Match the owner's pace: they ask short, direct questions and want short,
  direct answers with the finding stated up front.

---

## 7. Where to look in the OpenList source

```
server/handles/emby.go            DTO structs (embyItem, embyUserData), routing,
                                  embyTimeStr, embyTicks, auth, users
server/handles/emby_browse.go     the 4 DTO constructors, sortEmbyItems,
                                  filterEmbyItems, embySearchHints, library build
server/handles/emby_play.go       playbackinfo, progress, played/favorite
server/handles/media_user.go      web-UI progress/favorites/resume
server/handles/media_gate.go      MediaItemFilePath, access checks, signed links
internal/model/media.go           MediaItem, EpisodeInfo
internal/model/media_extra.go     MediaUserData, EmbyDevice
internal/db/media.go              CreateOrUpdateMediaItem, Delete/Clear
internal/conf/const.go            media_* settings keys
internal/bootstrap/data/setting.go  setting defaults + help text
internal/search/bleve/            OpenList's own VFS index
internal/model/obj.go             SortFiles (natural.Less — byte compare)
```
