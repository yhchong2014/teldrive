"""Prove the Chinese-search-enhanced matching logic that OpenList lacks.

Mirrors what StrmAssistant does server-side in Emby:
  1. fuzzy  : substring match on name
  2. pinyin : full pinyin and pinyin initials, e.g. ldlsw -> 流浪地球
  3. t2s    : traditional/simplified normalisation, 繁體 -> 繁体
"""
from pypinyin import lazy_pinyin, Style, load_phrases_dict

# 流浪地球 -> liu lang di qiu ; 让飘 -> rang piao
# Fix the classic polyphone trap: "流浪地球" would otherwise be
# liu lang di qiu (correct) but 重庆 must be chong qing, not zhong qing.
load_phrases_dict({
    "重庆": [["chóng"], ["qìng"]],
    "重庆森林": [["chóng"], ["qìng"], ["sēn"], ["lín"]],
    "银行": [["yín"], ["háng"]],
})

# Minimal traditional -> simplified map for the demo (opencc-go does this properly)
T2S = {"繁": "繁", "體": "体", "流": "流", "浪": "浪", "地": "地", "球": "球",
       "讓": "让", "飄": "飘", "重": "重", "慶": "庆", "森": "森", "林": "林",
       "這": "这", "個": "个", "殺": "杀", "手": "手"}


def t2s(s: str) -> str:
    return "".join(T2S.get(ch, ch) for ch in s)


def pinyin_forms(title: str) -> set[str]:
    full = "".join(lazy_pinyin(title, style=Style.NORMAL))
    initials = "".join(lazy_pinyin(title, style=Style.FIRST_LETTER))
    forms = {full.lower(), initials.lower()}
    # collapse spaces so "liu lang di qiu" also matches "liulangdiqiu"
    forms.add(full.replace(" ", "").lower())
    return forms


def matches(query: str, title: str) -> tuple[bool, str]:
    q = query.strip().lower()
    t = title.strip().lower()
    if not q:
        return True, "empty"
    # 1. plain substring (what OpenList already does)
    if q in t:
        return True, "substring"
    # 2. traditional/simplified normalisation
    if q in t2s(t) or t2s(q) in t:
        return True, "t2s"
    # 3. pinyin, full + initials; strip spaces so "liu lang" == "liulang"
    q_nospace = q.replace(" ", "")
    for form in pinyin_forms(title):
        if q in form or q_nospace in form.replace(" ", ""):
            return True, f"pinyin({form})"
    return False, "-"


CASES = [
    ("流浪地球", "流浪地球", True),
    ("liulang", "流浪地球", True),
    ("lldq", "流浪地球", True),
    ("ldlsw", "流浪地球", False),   # wrong initials must NOT match
    ("流浪", "流浪地球", True),
    ("liu lang", "流浪地球", True),
    ("重庆森林", "重庆森林", True),
    ("cqsl", "重庆森林", True),      # polyphone: chong not zhong
    ("zqsl", "重庆森林", False),     # wrong reading must not match
    ("繁体", "繁體", True),          # simplified query, traditional title
    ("繁體", "繁体", True),          # traditional query, simplified title
    ("让飘", "讓飄", True),
    ("殺手", "杀手", True),
    ("zxcvbn", "流浪地球", False),   # unrelated must not match
    ("Interstellar", "Interstellar", True),
    ("inter", "Interstellar", True),
]

print(f"{'query':<12} {'title':<12} {'ok':<4} {'expected':<9} match-via")
print("-" * 62)
fails = 0
for q, title, expected in CASES:
    got, how = matches(q, title)
    ok = got == expected
    fails += not ok
    print(f"{q:<12} {title:<12} {str(got):<4} {str(expected):<9} {how}")
print()
print("ALL PASS" if not fails else f"{fails} FAILED")
