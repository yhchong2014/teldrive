"""Show why a Chinese library can't sort correctly in OpenList, and what fixes it.

OpenList does:  SortName = Name   (verbatim copy, every single site)
and sorts with: strings.ToLower(SortName) < strings.ToLower(SortName)
i.e. a raw byte / UTF-8 codepoint comparison.

CJK codepoints are ordered by radical-and-stroke, NOT by pinyin, so Chinese
titles come out in a meaningless order that shifts as you add titles.
"""
from pypinyin import lazy_pinyin, Style

TITLES = [
    "流浪地球",
    "重庆森林",
    "杀手",
    "让子弹飞",
    "霸王别姬",
    "我不是药神",
    "大话西游",
    "无间道",
    "唐人街探案",
    "战狼",
]


def initials(title: str) -> str:
    return "".join(lazy_pinyin(title, style=Style.FIRST_LETTER)).upper()


print("How OpenList sorts today (SortName == Name, codepoint order):")
print("-" * 46)
for i, t in enumerate(sorted(TITLES), 1):
    print(f"  {i:2}. {t}   (U+{ord(t[0]):04X})")

print()
print("How it should sort (pinyin initials as SortName):")
print("-" * 46)
ranked = sorted(TITLES, key=initials)
for i, t in enumerate(ranked, 1):
    print(f"  {i:2}. {t:<10} SortName={initials(t)}")

# is either ordering stable/alphabetical for a human?
codepoint_order = sorted(TITLES)
pinyin_order = ranked
print()
print(f"same order? {codepoint_order == pinyin_order}")
print()
print("Mixed English + Chinese is worse - ASCII sorts before all CJK, so every")
print("Chinese title lands after every English one, regardless of pinyin:")
mixed = ["流浪地球", "Alien", "霸王别姬", "Dune", "战狼", "Interstellar"]
print("-" * 46)
for i, t in enumerate(sorted(mixed), 1):
    print(f"  {i:2}. {t}")
print()
print("with pinyin initials as SortName:")
print("-" * 46)
for i, t in enumerate(sorted(mixed, key=lambda s: initials(s) if ord(s[0]) > 0x2E80 else s.upper()), 1):
    key = initials(t) if ord(t[0]) > 0x2E80 else t.upper()
    print(f"  {i:2}. {t:<12} SortName={key}")
