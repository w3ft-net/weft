import os
import tempfile
import unittest

from canary import FileSink, SocketSink, row


class TestRow(unittest.TestCase):
    def test_the_body_carries_the_run_and_sequence(self):
        self.assertEqual(row("abc", 7, 123), "canary run=abc seq=7 sent_ns=123")


class TestFileSink(unittest.TestCase):
    def test_each_row_is_a_line_visible_to_a_tailer_at_once(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "roll-canary.log")
            sink = FileSink(path)
            sink.send("canary run=a seq=0 sent_ns=1")
            # Read through a second handle before closing the sink: a tailer
            # must see the line as soon as send returns.
            with open(path) as f:
                self.assertEqual(f.read(), "canary run=a seq=0 sent_ns=1\n")
            sink.send("canary run=a seq=1 sent_ns=2")
            sink.close()
            with open(path) as f:
                self.assertEqual(f.read().splitlines(), ["canary run=a seq=0 sent_ns=1", "canary run=a seq=1 sent_ns=2"])

    def test_a_write_that_fails_raises_so_the_row_is_not_counted(self):
        class Full:
            def write(self, _):
                raise OSError("No space left on device")

        with tempfile.TemporaryDirectory() as d:
            sink = FileSink(os.path.join(d, "x.log"))
            sink.f = Full()
            with self.assertRaises(OSError):
                sink.send("canary run=a seq=0 sent_ns=1")


class TestSocketSink(unittest.TestCase):
    def test_the_app_prefix_names_the_service(self):
        self.assertEqual(SocketSink.frame("canary run=a seq=0 sent_ns=1"), b"roll-canary:canary run=a seq=0 sent_ns=1\n")


if __name__ == "__main__":
    unittest.main()
