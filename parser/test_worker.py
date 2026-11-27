"""Worker protocol contracts without loading neural model weights."""
import unittest
from unittest.mock import patch
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
from parse_worker import Worker, clean, merge_words, surface_reading
from analyzers import SudachiAnalyzer


class WorkerTests(unittest.TestCase):
    def worker(self):
        w = Worker.__new__(Worker)
        w.backend, w.analyzer = 'sudachi', SudachiAnalyzer()
        w.baseline_only, w.version, w.vm = True, 'v1+rules', None
        w._component_parents = []
        w.model, w.tok, w.threshold = None, None, .68
        return w

    def test_pos_keep_and_legacy_row_fields(self):
        w = self.worker()
        tokens = w.annotate('私は食べた。123 XYZ')
        self.assertEqual(next(t for t in tokens if t['base'] == '食べる')['reading'], 'たべる')
        self.assertFalse(next(t for t in tokens if t['surface'] == '123')['keep'])
        self.assertFalse(next(t for t in tokens if t['surface'] == '。')['keep'])
        self.assertEqual(next(t for t in tokens if t['surface'] == '私')['keep'], 1)
        self.assertTrue(all({'p2', 'p3', 'lemma', 'keep', 'kana'} <= t.keys() for t in tokens))

    def test_live_surface_reading_and_offsets(self):
        text = '😀食べた。来た。'
        for token in self.worker().annotate(text):
            self.assertEqual(text[token['offset']:token['end']], token['surface'])
            if token['base'] == '食べる':
                self.assertEqual(surface_reading(token), 'たべ')
            if token['base'] == '来る':
                self.assertEqual(surface_reading(token), 'き')
        self.assertEqual(surface_reading(dict(reading=None)), '')
        self.assertEqual(surface_reading(dict(surface='私', base='私', reading='わたし', changed=True)), 'わたし')

    def test_unidic_fallback_keeps_legacy_merge_repairs(self):
        w = self.worker()
        w.backend = 'unidic'
        tokens = w.annotate('ごちそう')
        self.assertEqual(len(tokens), 1)
        self.assertEqual(tokens[0]['reading'], 'ごちそう')
        self.assertEqual(tokens[0]['analyzer_reading'], tokens[0]['unidic_reading'])
        self.assertEqual(surface_reading(tokens[0]), 'ごちそう')

    def test_word_merge(self):
        w = self.worker()
        surf = lambda text: [t['surface'] for t in merge_words(w.annotate(text))]
        self.assertEqual(surf('なんか、そういう話'), ['なんか', '、', 'そういう', '話'])
        self.assertEqual(surf('よろしくお願いします')[:2], ['よろしく', 'お願い'])
        self.assertIn('それ', surf('それに気づいた'))        # それ + に, not "besides"
        self.assertIn('自分', surf('自分で作った'))          # noun + case particle stays apart
        self.assertNotIn('した', surf('勉強した'))           # never a verb with its endings
        t = next(t for t in merge_words(w.annotate('お願いね')) if t['surface'] == 'お願い')
        self.assertEqual((t['base'], t['reading'], t['offset'], t['keep']), ('お願い', 'おねがい', 0, 1))

    def test_cleaning_contract(self):
        self.assertEqual(clean('<b>私</b>[わたし]（注）(note)\nです'), '私 です')


if __name__ == '__main__':
    unittest.main()
