"""Port of the Go pinyin.go logic, to validate behaviour before compiling.

Mirrors initialsWithPhrases / embySortName exactly, using pypinyin
(which wraps the same Unihan readings as mozillazg/go-pinyin).
"""

PHRASES = {
    "重庆": "chong qing", "重庆森林": "chong qing sen lin", "银行": "yin hang",
    "行长": "hang zhang", "长大": "zhang da", "长城": "chang cheng",
    "长安": "chang an", "长空": "chang kong", "长江": "chang jiang",
    "长沙": "chang sha", "长春": "chang chun", "音乐": "yin yue",
    "快乐": "kuai le", "乐队": "yue dui", "重新": "chong xin",
    "重复": "chong fu", "重量": "zhong liang", "重要": "zhong yao",
    "行为": "xing wei", "行业": "hang ye", "同行": "tong hang",
    "人参": "ren shen", "曾经": "ceng jing", "姓曾": "xing zeng",
}

from pypinyin import lazy_pinyin, Style


def is_han(ch):
    return "\u4e00" <= ch <= "\u9fff" or "\u3400" <= ch <= "\u4dbf"


def has_han(s):
    return any(is_han(c) for c in s)


def han_only(s):
    return "".join(c for c in s if is_han(c))


def initials_with_phrases(han):
    SENT = "\ue000"
    replaced, sb, rest = [], [], han
    while rest:
        matched = False
        for phrase in sorted(PHRASES, key=len, reverse=True):  # longest first
            if rest.startswith(phrase):
                sb.append(SENT)
                replaced.append(phrase)
                rest = rest[len(phrase):]
                matched = True
                break
        if not matched:
            sb.append(rest[0])
            rest = rest[1:]
    out, idx = [], 0
    for r in sb:
        if r == SENT:
            for p in PHRASES[replaced[idx]].split():
                out.append(p[:1].upper())
            idx += 1
            continue
        py = lazy_pinyin(r, style=Style.FIRST_LETTER)
        if py and py[0]:
            out.append(py[0].upper())
    return "".join(out)


def emby_sort_name(name):
    name = name.strip()
    if not name or not has_han(name):
        return name
    initials = initials_with_phrases(han_only(name))
    if not initials:
        return name
    # 英文前缀保留
    prefix = name.rstrip("".join(chr(c) for c in range(0x3000, 0x3040)))
    prefix = "".join(
        c for c in name
        if not (is_han(c) or c.isspace() or c in "：:，,。.！!？?-—~《》()（）[]【】·")
    ).strip()
    if prefix and not has_han(prefix):
        return prefix.upper() + " " + initials
    return initials


CASES = [
    # (input title, expected SortName)
    ("流浪地球", "LLDQ"),
    ("重庆森林", "CQSL"),          # polyphone: chong, not zhong
    ("杀手", "SS"),
    ("让子弹飞", "RZDF"),
    ("霸王别姬", "BWBJ"),
    ("我不是药神", "WBSYS"),
    ("大话西游", "DHXY"),
    ("无间道", "WJD"),
    ("唐人街探案", "TRJTA"),
    ("战狼", "ZL"),
    ("长城", "CC"),               # chang, not zhang
    ("长安十二时辰", "CASESC"),   # chang, and 十二 = shi-er = S E
    ("音乐之声", "YYZS"),          # yue, not le
    ("Alien", "Alien"),           # ASCII untouched
    ("Dune", "Dune"),
    ("", ""),
]

print(f"{'title':<14} {'got':<12} {'expected':<12} ok")
print("-" * 46)
fails = 0
for src, exp in CASES:
    got = emby_sort_name(src)
    ok = got == exp
    fails += not ok
    print(f"{src:<14} {got:<12} {exp:<12} {'OK' if ok else 'FAIL'}")
print()
print("ALL PASS" if not fails else f"{fails} FAILED")

print()
print("Library ordering with the patch applied:")
print("-" * 46)
lib = ["流浪地球", "重庆森林", "杀手", "霸王别姬", "战狼", "Alien", "Dune", "长城"]
for i, t in enumerate(sorted(lib, key=lambda s: emby_sort_name(s).lower()), 1):
    print(f"  {i}. {t:<10} SortName={emby_sort_name(t)}")
