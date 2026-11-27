"""JSONL tokenizer worker for Fazon-DB: Faizan's reading-disambiguation model.

Sudachi Full supplies primary word spans; the ModernBERT cross-encoder in
jev_parser_test/models/best picks the reading of every homograph token, with
the rd_config extras (deterministic rules, hybrid variant model) applied the
same way reading_disambig.Disambiguator does. jev_parser_test/word_merge.py then
joins pieces Sudachi over-splits (なん|か, そう|いう, お|願い) into one word.

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
import argparse, functools, json, os, re, sys, unicodedata

HERE = os.path.dirname(os.path.abspath(__file__))
PARSER_DIR = os.environ.get("MDB_PARSER_DIR", os.path.expanduser("~/Language Learning/jev_parser_test"))
MODEL_DIR = os.path.join(PARSER_DIR, "models", "best")
sys.path.insert(0, PARSER_DIR)

import reading_disambig as RD  # noqa: E402
import candgen as C  # noqa: E402
import word_merge as WM  # noqa: E402

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
    if not t.get("reading"):
        return ""
    if not t["changed"]:
        return t["kana"]
    surf, base, new = t["surface"], t["base"], t["reading"]
    if unicodedata.normalize("NFKC", surf) == unicodedata.normalize("NFKC", base):  # km vs ｋｍ
        return new
    # 行っ / 行く + おこなう -> おこなっ: swap the okurigana tail of the base form
    p = 0
    while p < min(len(surf), len(base)) and surf[p] == base[p]:
        p += 1
    btail, stail = base[p:], surf[p:]
    if C.KANJI.search(btail) or C.KANJI.search(stail) or not new.endswith(C.hira(btail)):
        return ""
    return new[:len(new) - len(btail)] + C.hira(stail)


@functools.lru_cache(maxsize=None)
def kinki_readings():
    """Kinki accent dictionary: spelling → readings (hiragana)."""
    import csv
    out = {}
    path = os.path.expanduser("~/Language Learning/Keihan/kinki_accent.merged.csv")
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            for row in csv.DictReader(f):
                for sp in re.split(r"[・／/,、 ]", row["元表記など"] or ""):
                    if sp:
                        out.setdefault(sp, set()).add(C.hira(row["単語"]))
    return out


def merged_reading(surface, joined, sudachi=None):
    """The merged word's reading. The pieces' readings joined, unless the dictionary lists the
    whole word and not that reading: 一昨|年 いっさく+とし → おととし, 一昨|日 → おととい."""
    listed = RD.compound_readings(surface)
    if not listed or C.norm(joined) in listed or joined in kinki_readings().get(surface, ()):
        return joined   # Kinki-listed casual/Kansai readings stay: 何も なんも
    lex = {C.hira(m.reading_form()) for m in sudachi.lookup(surface)
           if m.dictionary_form() == surface} if sudachi is not None else set()
    pick = {r for r in lex if C.norm(r) in listed} or lex or set(listed)
    if len(pick) > 1:  # 一昨日 おととい / いっさくじつ: take the common (priority-tagged) one
        pri = [r for rs, _, _ in C.entries(surface) for r in rs if r in pick]
        pick = {pri[0]} if pri else pick
    return pick.pop() if len(pick) == 1 else joined


def merge_words(toks, sudachi=None):
    """Join Sudachi pieces that make one learner word (なん|か → なんか, お|願い → お願い), per word_merge.
    The whole word is its own base form; its reading is the pieces' in-context readings joined.
    Pieces that aren't counted as words (names, numerals, symbols) or have no reading stay apart."""
    out = []
    for i, n in WM.spans([(t["surface"], t["pos"]) for t in toks]):
        ps = toks[i:i + n]
        # in-context reading of each piece: the model's when it changed one, else the analyzer's
        reads = [C.hira((surface_reading(p) if p.get("changed") else p.get("kana")) or "")
                 or (p["surface"] if WM.KANA.match(p["surface"]) else "") for p in ps]
        if n == 1 or not all(p.get("keep", 1) for p in ps) or not all(reads) or any(
                a["offset"] + len(a["surface"]) != b["offset"] for a, b in zip(ps, ps[1:])):
            out.extend(ps)
            continue
        surface = "".join(p["surface"] for p in ps)
        reading = merged_reading(surface, C.hira("".join(reads)), sudachi)
        pos = next((p["pos"] for p in ps if p["pos"] != "助詞"), ps[0]["pos"])
        out.append(dict(surface=surface, offset=ps[0]["offset"], end=ps[-1]["offset"] + len(ps[-1]["surface"]),
                        base=surface, reading=reading, kana=reading, changed=False, confidence=None, pos=pos,
                        p2="", p3="", keep=1, lemma=surface, merged=[p["surface"] for p in ps]))
    return out


class Worker(RD.Disambiguator):
    """Same as Disambiguator, but also records POS + keep flag per token."""

    def _tokens(self, text, prev, next, todo):
        if self.backend == 'unidic':
            tokens = self._legacy_tokens(text, prev, next, todo)
            for t in tokens:
                t.update(analyzer='unidic', analyzer_reading=t['unidic_reading'],
                         reading_source='rule' if t.get('rule') else 'analyzer',
                         end=t['offset'] + len(t['surface']))
            return tokens
        tokens = super()._tokens(text, prev, next, todo)
        for t in tokens:
            p = t['pos_detail']
            t.update(p2=p[1], p3=p[2], lemma=t['normalized'], kana=t['surface_reading'] or '')
            t['keep'] = int(t['pos'] not in SKIP_POS1 and p[1] not in {'数詞', '固有名詞'}
                            and not t['is_oov'] and not _NUM.search(t['surface']))
        # Keep the existing word filters for top-level model requests. Components
        # of a retained lexical unit remain available for its contextual reading.
        kept = {id(t) for t in tokens if t['keep']}
        primary = {id(t) for t in tokens}
        todo[:] = [x for x in todo if id(x[0]) not in primary or id(x[0]) in kept]
        return tokens

    def _legacy_tokens(self, text, prev, next, todo):
        toks, pos = [], 0
        ws = list(C.tagger()(text))
        before, before_pos = "", ""  # previous token (surface, pos1), for the rules that look back
        k = 0
        while k < len(ws):
            w, mg = ws[k], C.merge_span(ws, k)
            n = mg[0] if mg else 1
            surface = "".join(x.surface for x in ws[k:k + n])
            start = text.find(surface, pos)
            start = pos if start < 0 else start
            pos = start + len(surface)
            if mg:
                # a word UniDic split in two (上|がんねん, ご|ちそう): one token, reading from the merge table
                _, base, reading, kana, pos1 = mg
                t = dict(surface=surface, offset=start, base=base, pos=pos1, unidic_reading=reading,
                         reading=reading, changed=False, confidence=None, candidates=None,
                         p2="", p3="", lemma=base, keep=1, kana=kana)
                toks.append(t)
                before, before_pos = surface, pos1
                k += n
                continue
            base, uni = C.base_form(w), C.unidic_reading(w)
            t = dict(surface=w.surface, offset=start, base=base, pos=w.feature.pos1, unidic_reading=uni,
                     reading=uni, changed=False, confidence=None, candidates=None,
                     p2=w.feature.pos2 or "", p3=w.feature.pos3 or "",
                     lemma=(w.feature.lemma or "").split("-")[0] if w.feature.lemma not in (None, "*") else base)
            t["keep"] = keep(w, t)
            kana = w.feature.kana
            t["kana"] = C.hira(kana) if kana and kana != "*" else ""
            # same target selection as Disambiguator._tokens (rules, v1 homographs, hybrid variant targets),
            # with the model only run on tokens that count as words
            nxt = ws[k + 1].surface if k + 1 < len(ws) else ""
            rule = (C.rule_reading(base, w.surface, nxt, self.version, w.feature.pos1, before, before_pos)
                    if C.KANJI.search(w.surface) or base in C.CITATION else None)
            if rule:
                t.update(reading=rule, changed=C.norm(rule) != C.norm(uni), rule=True)
            elif t["keep"] and C.KANJI.search(w.surface) and (C.is_target(base, self.version, w.surface, nxt) or
                                                              self.vm and C.is_variant_target(base, w.surface, nxt)):
                hy = self.vm is not None and not C.is_homograph(base)  # hybrid: variant targets -> variant model
                cfg, extras = (self.vm[0], self.vm[1]) if hy else (self.cfg, self.extras)
                cands = RD.R.cand_list(base, uni, extras)
                if len(cands) >= 2:
                    todo.append((t, cands, RD.R.pairs((prev, text, start, w.surface, next), base, uni, cands,
                                                      cfg["hint"]), hy))
            toks.append(t)
            before, before_pos = w.surface, w.feature.pos1
            k += 1
        return toks


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--backend', choices=['sudachi', 'unidic'], default=os.environ.get('MDB_PARSER_BACKEND', 'sudachi'))
    ap.add_argument('--legacy-gate', action='store_true', help='disable the expanded reading gate')
    ap.add_argument('--device', choices=['cpu', 'cuda'])
    args = ap.parse_args()
    w = Worker(MODEL_DIR, backend=args.backend, device=args.device,
               expanded_gate=False if args.legacy_gate else None)
    out = sys.stdout
    out.write(json.dumps({"ready": True, "model": MODEL_DIR, "device": str(w.device), "analyzer": w.analyzer.provenance()}) + "\n")
    out.flush()
    for raw in sys.stdin:
        raw = raw.strip()
        if not raw:
            continue
        req = json.loads(raw)
        if "live" in req:
            text = req["live"] or ""
            toks = w.annotate(text) if text else []
            if w.backend != 'unidic':
                toks = merge_words(toks, w.analyzer.dictionary)
            out.write(json.dumps({"toks": [
                [t["surface"], t["offset"], t["base"], C.hira(t["reading"]) if t["reading"] and t["reading"] != "?" else "",
                 surface_reading(t), bool(t["changed"])] for t in toks]}, ensure_ascii=False) + "\n")
            out.flush()
            continue
        items = req.get("lines") or []
        texts = [clean(x.get("text", "")) for x in items]
        res = []
        for x, toks in zip(items, w.annotate_lines(texts)):
            if w.backend != 'unidic':  # the UniDic fallback keeps its own merge table (candgen.merge_span)
                toks = merge_words(toks, w.analyzer.dictionary)
            res.append({"id": x["id"], "toks": [
                [t["surface"], t["base"], C.hira(t["reading"]) if t["reading"] else "", t["pos"] or "",
                 t["p2"], t["p3"], t["keep"], t["lemma"]] for t in toks]})
        out.write(json.dumps({"results": res}, ensure_ascii=False) + "\n")
        out.flush()


if __name__ == "__main__":
    main()
