"""Writer side of the roll canary. Runs on a coordinator host.

Sends numbered rows to the local weftd socket as service `roll-canary`, one
every 1/rate seconds, so they take the ordinary ingest path: weftd ->
coordinator -> shard. The sequence number advances only after the socket
accepted the line, so a local socket failure never counts as weft losing a row.
Prints the final count; hand it to verify.py as --sent.

    python3 canary.py --run <id> --rate 50 --duration 900
"""

import argparse
import socket
import time

SOCKET = "/var/run/weftd.sock"


def connect():
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.connect(SOCKET)
    return s


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--run", required=True)
    ap.add_argument("--rate", type=float, default=50)
    ap.add_argument("--duration", type=float, required=True)
    a = ap.parse_args()

    conn = connect()
    seq = 0
    start = time.monotonic()
    print(f"start run={a.run} {time.strftime('%H:%M:%S', time.gmtime())}Z", flush=True)
    while time.monotonic() - start < a.duration:
        due = start + seq / a.rate
        time.sleep(max(0, due - time.monotonic()))
        line = f"roll-canary:canary run={a.run} seq={seq} sent_ns={time.time_ns()}\n"
        try:
            conn.sendall(line.encode())
        except OSError:
            conn = connect()
            continue
        seq += 1
    conn.close()
    print(f"done run={a.run} sent={seq} {time.strftime('%H:%M:%S', time.gmtime())}Z", flush=True)


if __name__ == "__main__":
    main()
