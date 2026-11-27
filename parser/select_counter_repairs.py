#!/usr/bin/env python3
"""Read-only selection of expanded-gate counter regressions against a snapshot."""
import argparse
import collections
import json
from pathlib import Path
import sqlite3
import sys

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO.parent.parent / 'jev_parser_test'))
from analyzers import SudachiAnalyzer
from parse_worker import clean
import candgen as C


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--before', type=Path, required=True)
    ap.add_argument('--out', type=Path, required=True)
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    analyzer = SudachiAnalyzer()
    con = sqlite3.connect((REPO / 'data/fazon.db').as_uri() + '?mode=ro', uri=True)
    con.execute('ATTACH DATABASE ? AS old', (args.before.resolve().as_uri() + '?mode=ro',))
    readings = dict(con.execute('SELECT id,reading FROM morphs'))
    ids, expected = [], []
    transitions = collections.Counter()
    skipped = 0
    rows = con.execute('''SELECT n.id,n.parsed_text,n.tokens FROM notes n JOIN old.notes o ON o.id=n.id
      WHERE n.parsed_text=o.parsed_text AND n.tokens!=o.tokens''')
    for nid, text, stored in rows:
        if not any(ch.isdigit() or ch in '〇零一二三四五六七八九十百千万億兆壱弐参' for ch in text):
            continue
        text = clean(text)
        tokens, stored = analyzer.tokens(text), json.loads(stored)
        if [t['surface'] for t in tokens] != [t[0] for t in stored]:
            skipped += 1
            continue
        changed = False
        for i, (token, (_, mid)) in enumerate(zip(tokens, stored)):
            if not mid or not C.KANJI.search(token['surface']) or not C.numeric_counter_guard(token, text):
                continue
            nxt = tokens[i + 1]['surface'] if i + 1 < len(tokens) else ''
            if C.is_target(token['base'], 'v1+rules', token['surface'], nxt) or C.is_variant_target(token['base'], token['surface'], nxt):
                continue
            before = tokens[i - 1] if i else None
            if C.rule_reading(token['base'], token['surface'], nxt, 'v1+rules', token['pos'],
                              before['surface'] if before else '', before['pos'] if before else ''):
                continue
            if readings.get(mid) == token['analyzer_reading']:
                continue
            expected.append(dict(note=nid, token_index=i, surface=token['surface'],
                                 old_reading=readings.get(mid), expected_reading=token['analyzer_reading']))
            transitions[(token['surface'], readings.get(mid), token['analyzer_reading'])] += 1
            changed = True
        if changed:
            ids.append(nid)
    (args.out / 'ids.txt').write_text(''.join(str(n) + '\n' for n in ids))
    (args.out / 'expected.json').write_text(json.dumps(expected, ensure_ascii=False, indent=2) + '\n')
    summary = dict(job_dir=str(args.out.resolve()), notes=len(ids), protected_occurrences=len(expected),
                   alignment_skips=skipped, transitions=[dict(surface=s, before=b, after=a, count=n)
                   for (s, b, a), n in transitions.most_common()])
    (args.out / 'selection.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps(summary, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
