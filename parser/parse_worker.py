"""JSONL tokenizer worker for mining-db: Faizan's reading-disambiguation model.

UniDic (fugashi) splits the text; the ModernBERT cross-encoder in
jev_parser_test/models/best picks the reading of every homograph token.

stdin, one JSON object per line:
    {"lines": [{"id": 123, "text": "..."}, ...]}
  The lines in one request are consecutive cues (one deck, in order); each
  line uses its neighbours as context, as reading_disambig.annotate_lines does.
stdout, one JSON object per request:
    {"results": [{"id": 123, "toks": [[surface, base, reading, pos1, pos2, pos3, keep, lemma], ...]}]}
  keep = 1 if the token counts as a word (morph), 0 if it is ignored the same
  way AnkiMorphs ignores it: symbols/punctuation/whitespace, numerals, proper
  nouns, and out-of-dictionary tokens.

Text preprocessing also follows the AnkiMorphs settings in use: HTML is
stripped and the contents of [..], （..） and (..) are dropped.

Live mode (for the /mvj-mine bridge's furigana, via mdb serve's /api/parse):
    {"live": "text"}  ->  {"toks": [[surface, offset, base, base_reading, surface_reading, changed], ...]}
  The text is used as given (not cleaned), so offsets match the caller's text.
  surface_reading is the in-context reading of the written form (飲ん -> のん,
  来 -> き), or "" when it can't be derived.
"""
import json, os, re, sys

HERE = os.path.dirname(os.path.abspath(__file__))
PARSER_DIR = os.path.expanduser("~/Language Learning/jev_parser_test")
MODEL_DIR = os.path.join(PARSER_DIR, "models", "best")
sys.path.insert(0, PARSER_DIR)

import reading_disambig as RD  # noqa: E402
import candgen as C  # noqa: E402

SKIP_POS1 = {"補助記号", "記号", "空白"}
_TAG = re.compile(r"<[^>]+>")
_BRACKETS = [re.compile(r"\[[^\]]*\]"), re.compile(r"（[^）]*）"), re.compile(r"\([^)]*\)")]
_NUM = re.compile(r"[0-9０-９]")


def clean(text):
    t = _TAG.sub("", text or "").replace("&nbsp;", " ")
    t = t.replace("&amp;", "&").replace("&lt;", "<").replace("&gt;", ">")
    for rx in _BRACKETS:
        t = rx.sub("", t)
    return re.sub(r"[\r\n  ]+", " ", t).strip()


def keep(w, tok):
    f = w.feature
    if f.pos1 in SKIP_POS1 or f.pos2 == "数詞" or f.pos2 == "固有名詞":
        return 0
    if w.is_unk:
        return 0
    if _NUM.search(tok["surface"]):
        return 0
    return 1


def surface_reading(t):
    """Reading of the written form, given the model's dictionary-form reading."""
    if not t["changed"]:
        return t["kana"]
    surf, base, new = t["surface"], t["base"], t["reading"]
    if surf == base:
        return new
    # 行っ / 行く + おこなう -> おこなっ: swap the okurigana tail of the base form
    p = 0
    while p < min(len(surf), len(base)) and surf[p] == base[p]:
        p += 1
    btail, stail = base[p:], surf[p:]
    if C.KANJI.search(btail) or C.KANJI.search(stail) or not new.endswith(C.hira(btail)):
        return ""
    return new[:len(new) - len(btail)] + C.hira(stail)


class Worker(RD.Disambiguator):
    """Same as Disambiguator, but also records POS + keep flag per token."""

    def _tokens(self, text, prev, next, todo):
        toks, pos = [], 0
        for w in C.tagger()(text):
            start = text.find(w.surface, pos)
            start = pos if start < 0 else start
            pos = start + len(w.surface)
            base, uni = C.base_form(w), C.unidic_reading(w)
            t = dict(surface=w.surface, offset=start, base=base, pos=w.feature.pos1, unidic_reading=uni,
                     reading=uni, changed=False, confidence=None, candidates=None,
                     p2=w.feature.pos2 or "", p3=w.feature.pos3 or "",
                     lemma=(w.feature.lemma or "").split("-")[0] if w.feature.lemma not in (None, "*") else base)
            t["keep"] = keep(w, t)
            k = w.feature.kana
            t["kana"] = C.hira(k) if k and k != "*" else ""
            if t["keep"] and C.KANJI.search(w.surface) and C.is_homograph(base):
                cands = RD.R.cand_list(base, uni, self.extras)
                if len(cands) >= 2:
                    todo.append((t, cands, RD.R.pairs((prev, text, start, w.surface, next), base, uni, cands,
                                                      self.cfg["hint"])))
            toks.append(t)
        return toks


def main():
    w = Worker(MODEL_DIR)
    out = sys.stdout
    out.write(json.dumps({"ready": True, "model": MODEL_DIR, "device": str(w.device)}) + "\n")
    out.flush()
    for raw in sys.stdin:
        raw = raw.strip()
        if not raw:
            continue
        req = json.loads(raw)
        if "live" in req:
            text = req["live"] or ""
            toks = w.annotate(text) if text else []
            out.write(json.dumps({"toks": [
                [t["surface"], t["offset"], t["base"], C.hira(t["reading"]) if t["reading"] != "?" else "",
                 surface_reading(t), bool(t["changed"])] for t in toks]}, ensure_ascii=False) + "\n")
            out.flush()
            continue
        items = req.get("lines") or []
        texts = [clean(x.get("text", "")) for x in items]
        res = []
        for x, toks in zip(items, w.annotate_lines(texts)):
            res.append({"id": x["id"], "toks": [
                [t["surface"], t["base"], C.hira(t["reading"]) if t["reading"] else "", t["pos"] or "",
                 t["p2"], t["p3"], t["keep"], t["lemma"]] for t in toks]})
        out.write(json.dumps({"results": res}, ensure_ascii=False) + "\n")
        out.flush()


if __name__ == "__main__":
    main()
