"""Writer side of the roll canary. Runs on a coordinator host.

Sends numbered rows to the local weftd socket as service `roll-canary`, one
every 1/rate seconds, so they take the ordinary ingest path: weftd ->
coordinator -> shard. The sequence number advances only after the socket
accepted the line, so a local socket failure never counts as weft losing a row.
Prints the final count; hand it to verify.py as --sent.

    python3 canary.py --run <id> --rate 50 --duration 900 [--via file|socket]
"""

import argparse
import socket
import time

SOCKET = "/var/run/weftd.sock"
LOG_FILE = "/opt/app/logs/roll-canary.log"


def row(run, seq, now_ns):
    return f"canary run={run} seq={seq} sent_ns={now_ns}"


class SocketSink:
    """weftd's records socket: best-effort, rows in flight when its stream
    breaks are not replayed."""

    def __init__(self):
        self.conn = self._connect()

    @staticmethod
    def _connect():
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(SOCKET)
        return s

    @staticmethod
    def frame(body):
        return f"roll-canary:{body}\n".encode()

    def send(self, body):
        try:
            self.conn.sendall(self.frame(body))
        except OSError:
            self.conn = self._connect()
            raise

    def close(self):
        self.conn.close()


class FileSink:
    """A file weftd tails: checkpointed, delivered at least once, so a row
    can only go missing if weft lost it."""

    def __init__(self, path):
        self.f = open(path, "ab", buffering=0)

    def send(self, body):
        self.f.write(f"{body}\n".encode())

    def close(self):
        self.f.close()


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--run", required=True)
    ap.add_argument("--rate", type=float, default=50)
    ap.add_argument("--duration", type=float, required=True)
    ap.add_argument("--via", choices=["file", "socket"], default="file",
                    help="file (default): a log file weftd tails; socket: weftd's records socket")
    a = ap.parse_args()

    sink = FileSink(LOG_FILE) if a.via == "file" else SocketSink()
    seq = 0
    start = time.monotonic()
    print(f"start run={a.run} via={a.via} {time.strftime('%H:%M:%S', time.gmtime())}Z", flush=True)
    while time.monotonic() - start < a.duration:
        due = start + seq / a.rate
        time.sleep(max(0, due - time.monotonic()))
        try:
            sink.send(row(a.run, seq, time.time_ns()))
        except OSError:
            continue
        seq += 1
    sink.close()
    print(f"done run={a.run} sent={seq} {time.strftime('%H:%M:%S', time.gmtime())}Z", flush=True)


if __name__ == "__main__":
    main()
