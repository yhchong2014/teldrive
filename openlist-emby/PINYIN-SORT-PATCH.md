# 拼音首字母排序 (pinyin initials sorting) for OpenList's Emby API

Makes Emby clients sort Chinese titles by pinyin instead of by Unicode codepoint.

**Status: patch written against `v4.2.8`, not compiled.** There is no Go toolchain
in this environment (no root, can't install). The algorithm and every test
expectation were validated by porting the Go logic to Python and checking 23
cases — see `pinyin_patch_validate.py`. Please run `go build ./... && go test
./server/handles/ -run Pinyin` before shipping.

## The problem

Every Emby DTO in OpenList copies the display name verbatim into `SortName`:

```go
SortName: name,   // movieItem, seriesItem, albumItem, songItem — all identical
```

and `sortEmbyItems` compares them as plain strings:

```go
case "sortname", "name", "seriessortname":
    la, lb := strings.ToLower(a.SortName), strings.ToLower(b.SortName)
```

`ForcedSortName` — which Emby prefers over `SortName` — is declared in the DTO
but **never assigned anywhere** in OpenList.

CJK codepoints are ordered by radical-and-stroke, not pinyin, so a Chinese
library comes out in an order that means nothing to a viewer, and it shifts as
titles are added. Mixed libraries are worse: every ASCII title sorts before
every CJK one, so all Chinese films land after all English ones.

```
before                          after
Alien                           Alien      (ALIEN)
Dune                            Dune       (DUNE)
Interstellar                    Interstellar
战狼                            霸王别姬     (BWBJ)
流浪地球                         重庆森林     (CQSL)
霸王别姬                         流浪地球     (LLDQ)
                                战狼        (ZL)
```

## The patch

Three files, +291/-4:

| file | change |
|---|---|
| `server/handles/pinyin.go` | new — conversion + polyphone overrides + cache |
| `server/handles/emby_browse.go` | 4 sites: `SortName` + `ForcedSortName` |
| `server/handles/pinyin_test.go` | new — 23 regression cases |

No client-side change is needed. Emby/Infuse/VidHub already sort by `SortName`,
so they pick this up immediately.

## Apply

```bash
git clone https://github.com/OpenListTeam/OpenList
cd OpenList
git checkout v4.2.8
git apply /path/to/pinyin-sort-v4.2.8.patch

go get github.com/mozillazg/go-pinyin@latest
go mod tidy
go build ./...
go test ./server/handles/ -run 'Pinyin|SortName'
```

Then rebuild/restart OpenList and refresh the library in the client.

Against `main` the same four hunks apply, but `seriesItem` / `albumItem` context
has drifted slightly — `git apply --3way` will merge it.

## Design decisions

- **`ForcedSortName` is set too**, not just `SortName`. Emby ranks
  `ForcedSortName` above `SortName`; OpenList never sent it, so this is a free
  hook that makes the ordering stick across clients.
- **Pure-ASCII titles are returned untouched.** No uppercasing, no rewriting —
  `Interstellar` stays `Interstellar`. This keeps the patch a no-op for
  non-Chinese libraries and skips the cache entirely.
- **Mixed titles keep the Latin prefix.** `Dune 沙丘` → `DUNE SQ`, so it files
  under D rather than disappearing into the Chinese block.
- **Digits and punctuation are stripped for conversion.** `go-pinyin`'s
  `Pinyin()` silently drops any rune that isn't in its dictionary, so
  `流浪地球2` would otherwise lose the `2`. We only convert the Han characters
  and leave the rest of the title alone.
- **Polyphones are overridden by phrase.** This is the part that actually
  matters: `go-pinyin`'s single-character table puts the most common reading
  first, so `重庆` alone gives `ZQ` (zhòng) and `长城` gives `ZC` (zhǎng) —
  both wrong. `pinyinPhrases` maps ~25 high-frequency film/music words
  (重庆森林, 长城, 长安, 银行, 音乐, 乐器, 快乐, 重新, 行为 …) with
  longest-match-first so 重庆森林 isn't split as 重庆+森林.
- **Results are cached** in a `sync.Map`. `loadEmbyLibrary` rebuilds the whole
  library per request and the same series/album names recur across DTOs.

## Known limitations

- 多音字 coverage is a curated list, not a full segmentation pass. Rare
  place names and person names may still use the wrong reading. Adding a phrase
  is a one-line change to `pinyinPhrases`.
- Traditional Chinese works (`繁體` characters have their own dictionary
  entries), but `長` → `CHANG` while `长` → `CHANG` only via the phrase table;
  if you mix both forms, add the traditional variants to `pinyinPhrases`.
- Sort scope is whatever the client requests. StrmAssistant defaults to
  `Movie, Collection, Series`; this patch covers movie/series/album/song,
  since those are the four DTO constructors OpenList has.
- Episodes and seasons keep numeric `SortName` (`S01E03`) — they should stay
  in episode order, not alphabetical.
- This patch does **not** touch OpenList's own web UI file browser. That path
  uses `model.SortFiles` → `github.com/maruel/natural`, whose `commonPrefix`
  compares **bytes**, so it has the same CJK problem. Fixing it is a separate
  change (swap `natural.Less` for a pinyin-aware comparator when `hasHan`).
