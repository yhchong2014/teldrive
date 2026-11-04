#!/usr/bin/env python3
"""
Set OpenList's "date added" (Emby DateCreated) from each item's release date.

Why this exists
---------------
OpenList's Emby-compatible API emits:

    DateCreated  = media_items.created_at      # when the SCANNER first indexed it
    PremiereDate = media_items.release_date    # the real release date

Emby clients sort by DateCreated (sort key `DateCreated`), and there is no
`PremiereDate` sort key in OpenList's sortEmbyItems(), so a client-side
"Release Date" sort silently falls back to the pre-sort order. That is why a
freshly scanned library appears in arbitrary order and "Recently Added" is
meaningless.

There is no UI field and no API field for created_at. This script edits it
directly. Both DateCreated and PremiereDate are emitted, so after running this:

  - DateCreated  == release date  -> "Release Date" sorting actually works
  - PremiereDate == release date  -> both agree, no confusing mismatch

Usage
-----
    # dry run (the default) - shows exactly what would change
    python3 set_date_added.py --db /path/to/data/data.db

    # write the changes
    python3 set_date_added.py --db /path/to/data/data.db --apply

    # only movies (default), or everything
    python3 set_date_added.py --db ... --media-type video --apply
    python3 set_date_added.py --db ... --media-type all   --apply

    # MySQL / Postgres instead of the default sqlite file
    python3 set_date_added.py --backend mysql \
        --dsn "host=127.0.0.1 user=openlist password=... dbname=openlist" --apply

Stop OpenList before running: sqlite is a single file and OpenList may hold a
cached copy of the changed rows.
"""

from __future__ import annotations

import argparse
import os
import shutil
import sqlite3
import sys
from datetime import datetime, timezone

# Stored as noon UTC on purpose: OpenList renders DateCreated with .UTC(), so a
# midnight timestamp could slip back one calendar day for negative UTC offsets.
# Noon can never cross a day boundary in any timezone (-12..+14).
DEFAULT_TIME = "12:00:00"

# glebarez/go-sqlite (OpenList's sqlite driver) writes time.Time with
# parseTimeFormats[0] = "2006-01-02 15:04:05.999999999-07:00".
SQLITE_LAYOUT = "%Y-%m-%d %H:%M:%S+00:00"


# ---------------------------------------------------------------- date helpers

def build_datetime(date_str: str, clock: str) -> datetime:
    d = datetime.strptime(date_str.strip(), "%Y-%m-%d")
    parts = (clock.split(":") + ["0", "0"])[:3]
    return datetime(
        d.year, d.month, d.day, int(parts[0]), int(parts[1]), int(parts[2]),
        tzinfo=timezone.utc,
    )


def normalise_date(raw: str) -> str:
    """Accept '2009-11-13', '2009-11-13T00:00:00Z', '2009/11/13'. -> YYYY-MM-DD or ''."""
    raw = (raw or "").strip()
    if not raw:
        return ""
    raw = raw.replace("/", "-").replace("T", " ").split(" ")[0]
    try:
        return datetime.strptime(raw, "%Y-%m-%d").strftime("%Y-%m-%d")
    except ValueError:
        pass
    if len(raw) == 4 and raw.isdigit():  # bare year
        return f"{raw}-01-01"
    return ""


# ------------------------------------------------------------------- backends

class Backend:
    """Thin wrapper so the same logic works on sqlite / mysql / postgres."""

    name = "sqlite"
    placeholder = "?"
    comments = False  # MySQL can't reference an updated column inline (see below)

    def __init__(self, conn, prefix: str):
        self.conn = conn
        self.prefix = prefix

    @property
    def table(self) -> str:
        return f"{self.prefix}media_items"

    def columns(self) -> set[str]:
        raise NotImplementedError

    def render(self, dt: datetime):
        """Value to bind for a DATETIME column."""
        raise NotImplementedError

    def select_rows(self, media_types, include_hidden):
        sql = (
            f"SELECT id, created_at, release_date, scraped_name, file_name, "
            f"media_type, folder_path, is_folder FROM {self.table} "
            f"WHERE deleted_at IS NULL"
        )
        params: list = []
        if media_types:
            sql += " AND media_type IN (%s)" % ",".join([self.placeholder] * len(media_types))
            params += list(media_types)
        if not include_hidden and "hidden" in self.columns():
            sql += " AND (hidden = 0 OR hidden IS NULL)"
        sql += " ORDER BY release_date IS NULL, release_date, id"

        cur = self.conn.execute(sql, params)
        names = [d[0] for d in cur.description]
        rows = []
        for raw in cur:
            row = dict(zip(names, raw))
            row["created_at"] = str(row["created_at"])
            rows.append(row)
        return rows

    def apply_updates(self, updates) -> None:
        raise NotImplementedError


class SqliteBackend(Backend):
    name = "sqlite"

    def columns(self) -> set[str]:
        return {r[1] for r in self.conn.execute(f"PRAGMA table_info({self.table})")}

    def render(self, dt: datetime) -> str:
        return dt.strftime(SQLITE_LAYOUT)

    def select_rows(self, media_types, include_hidden):
        self.conn.row_factory = sqlite3.Row
        rows = super().select_rows(media_types, include_hidden)
        return rows

    def apply_updates(self, updates) -> None:
        with self.conn:
            self.conn.executemany(
                f"UPDATE {self.table} SET created_at = ? WHERE id = ?",
                [(new, item_id) for item_id, new, *_ in updates],
            )


class MysqlBackend(Backend):
    name = "mysql"

    def columns(self) -> set[str]:
        cur = self.conn.execute(
            "SELECT COLUMN_NAME FROM information_schema.COLUMNS "
            "WHERE TABLE_NAME = %s",
            (self.table,),
        )
        return {r[0].lower() for r in cur}

    def render(self, dt: datetime) -> datetime:
        return dt.replace(tzinfo=None)  # DATETIME column, store naive UTC

    def apply_updates(self, updates) -> None:
        # KNOWN LIMITATION: MySQL evaluates SET clauses left-to-right, so
        # "SET created_at=?, updated_at=created_at" would copy the NEW value.
        # updated_at is deliberately left untouched here.
        with self.conn:
            self.conn.executemany(
                f"UPDATE {self.table} SET created_at = %s WHERE id = %s",
                [(new, item_id) for item_id, new, *_ in updates],
            )


class PostgresBackend(Backend):
    name = "postgres"

    def columns(self) -> set[str]:
        cur = self.conn.execute(
            "SELECT column_name FROM information_schema.columns WHERE table_name = %s",
            (self.table,),
        )
        return {r[0].lower() for r in cur}

    def render(self, dt: datetime) -> datetime:
        return dt

    def apply_updates(self, updates) -> None:
        with self.conn:
            self.conn.executemany(
                f"UPDATE {self.table} SET created_at = %s WHERE id = %s",
                [(new, item_id) for item_id, new, *_ in updates],
            )


def connect_sqlite(path: str) -> sqlite3.Connection:
    conn = sqlite3.connect(path)
    conn.row_factory = sqlite3.Row
    return conn


def connect_mysql(dsn: str):
    try:
        import pymysql  # noqa: PLC0415
    except ImportError:
        sys.exit("error: --backend mysql needs PyMySQL:  pip install pymysql")
    # accept a DSN string or a URL
    if "://" in dsn:
        from urllib.parse import urlparse  # noqa: PLC0415

        u = urlparse(dsn)
        return pymysql.connect(
            host=u.hostname or "127.0.0.1", port=u.port or 3306,
            user=u.username or "root", password=u.password or "",
            database=(u.path or "/").lstrip("/"), charset="utf8mb4",
        )
    return pymysql.connect(**dict(
        kv.split("=", 1) for kv in dsn.replace(",", " ").split()
    ), charset="utf8mb4")


def connect_postgres(dsn: str):
    try:
        import psycopg2  # noqa: PLC0415
    except ImportError:
        sys.exit("error: --backend postgres needs psycopg2:  pip install psycopg2-binary")
    if "://" in dsn:
        import psycopg2.extensions  # noqa: F401,PLC0415

        return psycopg2.connect(dsn)
    return psycopg2.connect(**dict(
        kv.split("=", 1) for kv in dsn.replace(",", " ").split()
    ))


# ---------------------------------------------------------------------- logic

def plan(rows, clock: str, fallback_date: str):
    updates, skipped = [], []
    for row in rows:
        release = normalise_date(row["release_date"])
        if release:
            date_used, reason = release, "release_date"
        elif fallback_date:
            date_used, reason = fallback_date, "fallback"
        else:
            skipped.append((row, "no release_date"))
            continue
        new_dt = build_datetime(date_used, clock)
        # Backend-agnostic "already correct?" check: the first 19 chars are
        # "YYYY-MM-DD HH:MM:SS" for sqlite text and for mysql/pg DATETIME alike.
        old = str(row["created_at"])[:19]
        if old == new_dt.strftime("%Y-%m-%d %H:%M:%S"):
            skipped.append((row, "already correct"))
            continue
        updates.append((row["id"], new_dt, date_used, reason, row))
    return updates, skipped


def main() -> int:
    ap = argparse.ArgumentParser(
        description="Set OpenList media_items.created_at (= Emby DateCreated) from release_date.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("--backend", choices=["sqlite", "mysql", "postgres"], default="sqlite")
    ap.add_argument("--db", default="data/data.db", help="sqlite file (default: data/data.db)")
    ap.add_argument("--dsn", default="", help="connection string for mysql / postgres")
    ap.add_argument("--apply", action="store_true", help="write changes (default is a dry run)")
    ap.add_argument("--prefix", default="x_", help="table prefix (default: x_)")
    ap.add_argument(
        "--media-type", default="video",
        help="comma separated: video,music,image,book (default: video; 'all' for everything)",
    )
    ap.add_argument("--time", default=DEFAULT_TIME,
                    help=f"time-of-day to store, UTC (default {DEFAULT_TIME})")
    ap.add_argument("--fallback", default="",
                    help="YYYY-MM-DD for items with no release_date (default: skip them)")
    ap.add_argument("--include-hidden", action="store_true", help="also touch hidden items")
    ap.add_argument("--no-backup", action="store_true", help="skip the automatic .bak copy")
    args = ap.parse_args()

    if args.fallback:
        args.fallback = normalise_date(args.fallback)
        if not args.fallback:
            sys.exit("error: --fallback must be YYYY-MM-DD")

    media_types = (
        [] if args.media_type.lower() == "all"
        else [m.strip() for m in args.media_type.split(",") if m.strip()]
    )

    # ---- connect
    if args.backend == "sqlite":
        if not os.path.isfile(args.db):
            print(f"error: no such file: {args.db}", file=sys.stderr)
            print("hint: in Docker use <openlist-data-volume>/data/data.db", file=sys.stderr)
            return 2
        if args.apply and not args.no_backup:
            backup = f"{args.db}.bak-{datetime.now():%Y%m%d%H%M%S}"
            shutil.copy2(args.db, backup)
            print(f"backup written: {backup}")
        conn = connect_sqlite(args.db)
        backend = SqliteBackend(conn, args.prefix)
    elif args.backend == "mysql":
        if not args.dsn:
            sys.exit("error: --backend mysql needs --dsn")
        conn = connect_mysql(args.dsn)
        backend = MysqlBackend(conn, args.prefix)
    else:
        if not args.dsn:
            sys.exit("error: --backend postgres needs --dsn")
        conn = connect_postgres(args.dsn)
        backend = PostgresBackend(conn, args.prefix)

    # ---- read
    try:
        rows = backend.select_rows(media_types, args.include_hidden)
    except Exception as exc:  # noqa: BLE001
        print(f"error: {exc}", file=sys.stderr)
        print("hint: wrong --prefix? list tables and check for <prefix>media_items",
              file=sys.stderr)
        return 2

    if not rows:
        print("no matching items (is the media library scanned and enabled?)")
        return 0

    updates, skipped = plan(rows, args.time, args.fallback)

    print(f"\nbackend       : {backend.name}  (table {backend.table})")
    print(f"items matched : {len(rows)}")
    print(f"to change     : {len(updates)}")
    print(f"skipped       : {len(skipped)}")

    if updates:
        updates.sort(key=lambda u: u[2])  # by date, easiest to eyeball
        width = max(len((u[4]["scraped_name"] or u[4]["file_name"] or "")[:46]) for u in updates)
        width = max(width, 4)
        print()
        print(f"{'id':>6}  {'name':<{width}}  {'date':<10}  {'old created_at':<22} -> new")
        print("-" * (width + 58))
        for item_id, new_dt, date_used, reason, row in updates:
            name = (row["scraped_name"] or row["file_name"] or "")[:width]
            old = str(row["created_at"])[:19]
            tag = "" if reason == "release_date" else "  (fallback)"
            print(f"{item_id:>6}  {name:<{width}}  {date_used:<10}  {old:<22} -> "
                  f"{new_dt:%Y-%m-%d %H:%M}{tag}")

    no_date = [s for s in skipped if s[1] == "no release_date"]
    if no_date:
        print(f"\n{len(no_date)} item(s) have no release_date and will be left alone:")
        for row, _ in no_date[:15]:
            print(f"  id={row['id']:<6} {(row['scraped_name'] or row['file_name'])[:60]}")
        if len(no_date) > 15:
            print(f"  ... and {len(no_date) - 15} more")
        print("  -> scrape them, or pass --fallback YYYY-MM-DD")

    if not args.apply:
        print("\nDRY RUN - nothing written. Re-run with --apply to commit.")
        return 0

    if not updates:
        print("\nnothing to do.")
        return 0

    # ---- write
    rendered = [(backend.render(dt), item_id) for item_id, dt, *_ in updates]
    try:
        if isinstance(backend, SqliteBackend):
            with backend.conn:
                backend.conn.executemany(
                    f"UPDATE {backend.table} SET created_at = ? WHERE id = ?", rendered
                )
        elif isinstance(backend, MysqlBackend):
            with backend.conn:
                backend.conn.executemany(
                    f"UPDATE {backend.table} SET created_at = %s WHERE id = %s", rendered
                )
        else:
            with backend.conn:
                backend.conn.executemany(
                    f"UPDATE {backend.table} SET created_at = %s WHERE id = %s", rendered
                )
    except Exception as exc:  # noqa: BLE001
        print(f"\nerror writing: {exc}", file=sys.stderr)
        return 1

    print(f"\nupdated {len(rendered)} row(s).")
    print("\nNext steps:")
    print("  1. start OpenList")
    print("  2. POST /Library/Refresh  (or the client's 'Refresh metadata')")
    print("  3. in the client, sort the library by 'Release Date'")
    print("\nNote: always sort by 'Release Date'. OpenList has no PremiereDate sort")
    print("key, so that picker entry silently falls back to the unsorted order.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
