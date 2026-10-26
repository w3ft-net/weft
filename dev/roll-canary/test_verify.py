import unittest

from verify import account, parse_seq


class TestAccount(unittest.TestCase):
    def test_every_sequence_number_exactly_once_is_clean(self):
        r = account(sent=5, received=[0, 1, 2, 3, 4])
        self.assertEqual((r.missing, r.duplicates), ([], {}))
        self.assertTrue(r.ok)

    def test_a_gap_is_reported_as_missing(self):
        r = account(sent=6, received=[0, 1, 4, 5])
        self.assertEqual(r.missing, [2, 3])
        self.assertFalse(r.ok)

    def test_a_repeat_is_reported_with_its_count(self):
        r = account(sent=3, received=[0, 1, 1, 1, 2])
        self.assertEqual(r.duplicates, {1: 3})
        self.assertFalse(r.ok)

    def test_a_row_past_the_last_sent_is_not_silently_accepted(self):
        r = account(sent=2, received=[0, 1, 7])
        self.assertEqual(r.unexpected, [7])
        self.assertFalse(r.ok)


class TestParseSeq(unittest.TestCase):
    def test_reads_seq_for_this_run_only(self):
        self.assertEqual(parse_seq("canary run=abc seq=41 sent_ns=9", "abc"), 41)
        self.assertIsNone(parse_seq("canary run=other seq=41 sent_ns=9", "abc"))
        self.assertIsNone(parse_seq("unrelated line", "abc"))


if __name__ == "__main__":
    unittest.main()
