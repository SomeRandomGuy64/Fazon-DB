"""Which (base, reading) words are real dictionary words?

A kanji word is flagged only when a dictionary lists its exact spelling but not
its reading (冷蔵庫 れいぞうくら, 上 あ). Spellings no dictionary lists — compounds,
potential forms, slang (強え), Kansai coinages — are never flagged. Dictionaries: JMdict, KANJIDIC (single kanji: on'yomi and kun'yomi without
okurigana, so 分 わ — only the stem of わ.かる — is NOT a word), or the Kinki
accent dictionary (Kansai words). Sound changes are tolerated: rendaku /
handakuten on the first kana (分 ぷん, 所 どころ) and a final small っ (一 いっ).
Kana-only words are never checked: most Kansai-specific words are kana.

Not part of the parser fingerprint: changing this never forces a reparse.

usage: python3 dict_words.py DB [--dry-run]
  writes table nondict_words(base, reading) = words the engine ignores by default
"""
import csv, os, re, sqlite3, sys
from jamdict import Jamdict

KANJI = re.compile(r"[一-鿿々〆ヶ]")
KINKI = os.path.expanduser("~/Language Learning/Keihan/kinki_accent.merged.csv")
VOICED = str.maketrans("がぎぐげござじずぜぞだぢづでどばびぶべぼぱぴぷぺぽ",
                       "かきくけこさしすせそたちつてとはひふへほはひふへほ")

def hira(s):
    return "".join(chr(ord(c) - 0x60) if "ァ" <= c <= "ヶ" else c for c in s)

VOWEL = {c: v for v, row in zip("あいうえお", ("あかさたなはまやらわがざだばぱゃ", "いきしちにひみりぎじぢびぴ",
         "うくすつぬふむゆるぐずづぶぷゅ", "えけせてねへめれげぜでべぺ", "おこそとのほもよろをごぞどぼぽょ")) for c in row}

def long_vowels(r):
    """Kinki-dictionary ー → the vowel before it (いー → いい)."""
    out = ""
    for c in r:
        out += VOWEL.get(out[-1:], "ー") if c == "ー" and out else c
    return out

O_ROW = dict(zip("あかさたなはまやらわ", "おこそとのほもよろお"))

def keys(r):
    """The reading plus its sound-change-free forms."""
    r = long_vowels(hira(r)).translate(VOICED)  # rendaku anywhere: 何本 なんぼん, 分 ぷん
    out = {r}
    if r.endswith("っ"):
        out |= {r[:-1] + e for e in "つちくき"}
    if r.endswith("う") and len(r) >= 2:  # Kansai u-onbin: 使う つこう, 構う かもう, 言う ゆう
        a = r[-2]
        o = O_ROW.get(a)
        if o:
            out.add(r[:-2] + o + "う")
        if r.endswith("いう"):
            out.add(r[:-2] + "ゆう")
    return out

class Dict:
    """Loads JMdict, KANJIDIC and the Kinki dictionary into memory in one pass."""
    def __init__(self):
        from collections import defaultdict
        jm, kinki = defaultdict(set), defaultdict(set)
        c = sqlite3.connect(Jamdict().db_file)
        for k, r in c.execute("SELECT k.text, n.text FROM Kanji k JOIN Kana n ON n.idseq = k.idseq"):
            jm[k].add(hira(r))
        for lit, v in c.execute("""SELECT ch.literal, r.value FROM character ch JOIN rm_group g ON g.cid = ch.ID
                                   JOIN reading r ON r.gid = g.ID WHERE r.r_type IN ('ja_on', 'ja_kun')"""):
            if "." not in v:
                jm[lit].add(hira(v.strip("-")))
        with open(KINKI, encoding="utf-8") as f:
            for row in csv.DictReader(f):
                for sp in re.split(r"[・／/,、 ]", row["元表記など"] or ""):
                    if sp:
                        kinki[sp].add(hira(row["単語"]))
        self.jm, self.kinki = jm, kinki
        self.cache = {}

    def readings(self, base):
        if base not in self.cache:
            self.cache[base] = {k for r in self.jm.get(base, ()) for k in keys(r)}
            self.cache[base] |= {k for r in self.kinki.get(base, ()) for k in keys(r)}
        return self.cache[base]

    def real(self, base, reading, lemma=None):
        if not KANJI.search(base):
            return True
        have = self.readings(base)
        if not self.jm.get(base):  # no dictionary lists this spelling (compound, potential form, slang): can't judge
            return True
        return bool(keys(reading) & (have | (self.readings(lemma) if lemma and lemma != base else set())))

def main():
    db = sqlite3.connect(sys.argv[1], timeout=30)
    dry = "--dry-run" in sys.argv
    d = Dict()
    rows = db.execute("SELECT base, reading, COALESCE(lemma,''), freq, known FROM morphs").fetchall()
    bad = [(b, r, f, k) for b, r, l, f, k in rows if not d.real(b, r, l)]
    print(f"morphs {len(rows)}  non-dictionary {len(bad)}  "
          f"in use {sum(f > 0 for _, _, f, _ in bad)}  known {sum(k for *_, k in bad)}  "
          f"occurrences {sum(f for _, _, f, _ in bad)}")
    if dry:
        for b, r, f, k in sorted(bad, key=lambda x: -x[2]):
            print(f"{f}\t{b}\t{r}\t{'KNOWN' if k else ''}")
        return
    with db:
        db.execute("CREATE TABLE IF NOT EXISTS nondict_words(base TEXT NOT NULL, reading TEXT NOT NULL, PRIMARY KEY(base, reading))")
        db.execute("DELETE FROM nondict_words")
        db.executemany("INSERT INTO nondict_words VALUES(?,?)", [(b, r) for b, r, _, _ in bad])

if __name__ == "__main__":
    main()
