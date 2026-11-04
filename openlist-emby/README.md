# OpenList → Emby: set "Date Added" from the release date

OpenList's Emby-compatible API derives an item's **Date Added** from
`media_items.created_at`, which is the moment the **scanner first indexed** the
file — not the file's real date, and not its release date.

```
DateCreated  = media_items.created_at     <- what Emby sorts as "Date Added"
PremiereDate = media_items.release_date   <- the real release date
```

There is no UI field and no API field for `created_at`. This script writes it
directly, setting it to the item's `release_date`, so that sorting by
**Release Date** in any Emby client produces true chronological order.

## Why this is needed

OpenList's `sortEmbyItems()` supports these sort keys:

```
SortName · Name · SeriesSortName · DateCreated · DatePlayed
ProductionYear · ParentIndexNumber · IndexNumber · CommunityRating
```

There is **no `PremiereDate` / `ReleaseDate` key**. A client sending
`SortBy=PremiereDate` hits no matching case, so ordering is unaffected — it
returns items in whatever order they were assembled, which looks random.

Sorting by `DateCreated` *is* implemented, which is why rewriting `created_at`
to hold the release date is the way to get release-date order.

Because OpenList emits both `DateCreated` and `PremiereDate` from the same
underlying values, after this script both fields agree. You avoid the classic
"Recently Added shows 2009-11-13" oddity of a half-applied fix.

## Usage

```bash
# 1. dry run — always start here
python3 set_date_added.py --db /path/to/openlist/data/data.db

# 2. apply (writes a timestamped .bak first)
python3 set_date_added.py --db /path/to/openlist/data/data.db --apply

# 3. restart OpenList, then POST /Library/Refresh or use "Refresh metadata"
```

Docker users: the DB is inside the data volume, `/opt/openlist/data/data.db`.

```bash
docker compose stop openlist
docker run --rm -v openlist-data:/data -v "$PWD":/w python:3-slim \
  python /w/set_date_added.py --db /data/data.db --apply
docker compose start openlist
```

### Options

| Flag | Meaning |
|---|---|
| `--backend sqlite\|mysql\|postgres` | default `sqlite` |
| `--db PATH` | sqlite file, default `data/data.db` |
| `--dsn "..."` | connection string for mysql/postgres |
| `--apply` | write changes; without it everything is a dry run |
| `--media-type` | `video` (default), `music`, `image`, `book`, or `all` |
| `--prefix` | table prefix, default `x_` |
| `--time` | time-of-day stored, default `12:00:00` UTC |
| `--fallback YYYY-MM-DD` | date for items with no `release_date` (default: skip them) |
| `--include-hidden` | also process hidden items |
| `--no-backup` | skip the `.bak` copy |

## Behaviour worth knowing

- **Noon UTC, not midnight.** OpenList renders `DateCreated` with `.UTC()`. A
  midnight timestamp can slip to the previous calendar day in negative UTC
  offsets; noon can never cross a day boundary anywhere.
- **Only items that have a `release_date`** are changed by default. Unscraped
  items keep their scan time and are listed at the end of the report.
- **Safe to re-run.** Already-correct rows are skipped.
- **Values round-trip correctly.** The timestamp is written in exactly the
  layout OpenList's sqlite driver produces —
  `2006-01-02 15:04:05.999999999-07:00` — so Go parses it back into a real
  `time.Time` rather than a string.
- **`updated_at` is left alone.** OpenList uses it for the Subsonic `Starred`
  field; overwriting it would silently corrupt that.
- **Stop OpenList first.** It holds the sqlite file.

## Caveats

- Editing `created_at` makes **Date Added equal Release Date**. That is what
  makes release-order sorting work, but it does mean "Recently Added" stops
  meaning anything.
- This is a **local database edit**, not a feature. Every full rescan keeps the
  existing `created_at` (OpenList preserves it on upsert), so the change
  survives rescans — but *deleting* library items and re-adding them assigns
  fresh timestamps, and you would need to re-run this script.
- **TV episodes have no release date.** OpenList emits `PremiereDate` only for
  movies (`movieItem`); episodes carry only `DateCreated`, all inherited from
  the one merged show record. Running this on a TV library sets every episode
  to the show's release date. Season/episode ordering inside a series still
  comes from `ParentIndexNumber` / `IndexNumber`, so this is harmless — just
  don't expect per-episode air dates.
- Only meaningful on **`beta`** if you use merged multi-season folders: v4.2.8's
  `ep-<item>-<episode>` IDs collide across seasons.
