#!/usr/bin/env python3
"""
capture-render-syslog.py — a TLS syslog sink for reverse-engineering
Render.com's log-stream wire format. Pre-implementation tool for
docs/plans/render.md; delete once the Render adapter has shipped.

Render log streams only speak RFC5424 syslog over TLS/TCP (their HTTPS delivery
is endpoint-locked to Datadog/Loggly). Before we design weft's Render adapter we
need to know, empirically:

  1. Framing: octet-counted (RFC 6587 "3.4.1") vs. LF-delimited ("non-transparent")
  2. Whether/how an auth token reaches a *generic* (non-provider) endpoint
     — is it in STRUCTURED-DATA, and under what SD-ID / enterprise number?
  3. Exact PRI / VERSION / TIMESTAMP / HOSTNAME / APP-NAME / PROCID / MSGID
     contents across service types (web, cron, worker, pserv) and datastores
  4. Batching: one frame per write, or many? one connection or many?
  5. Multi-line handling (stack traces split or joined?)
  6. Reconnect / keepalive behaviour

This script terminates TLS, writes every received byte-chunk to a JSONL file
(base64, timestamped, with recv() boundaries preserved), and on shutdown prints
a framing + RFC5424 analysis. It never ACKs anything back — syslog/TCP has no
app-level ack, and Render doesn't expect one.

Usage
-----
  # Render validates the cert chain — you need a real one. Self-signed is
  # only good for exercising the parser locally.
  python3 capture-render-syslog.py --port 6514 \
      --cert /etc/letsencrypt/live/render-capture.w3ft.net/fullchain.pem \
      --key  /etc/letsencrypt/live/render-capture.w3ft.net/privkey.pem

  # local parser check only (Render will reject this cert):
  python3 capture-render-syslog.py --port 6514

Then in the Render dashboard: Workspace → Settings → Log Streams (or
Integrations → Observability) → add endpoint  render-capture.w3ft.net:6514
with (and later without) a token, and redeploy a noisy service.

Analyse afterwards without a live connection:
  python3 capture-render-syslog.py --analyze capture-YYYYmmdd-HHMMSS.jsonl
"""

import argparse
import base64
import datetime as dt
import json
import os
import re
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading

# ── raw capture ────────────────────────────────────────────────────────────────


def _ts():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def _selfsigned():
    """Generate an ephemeral self-signed cert; return (certfile, keyfile)."""
    d = tempfile.mkdtemp(prefix="render-capture-")
    cert = os.path.join(d, "cert.pem")
    key = os.path.join(d, "key.pem")
    subprocess.run(
        [
            "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
            "-keyout", key, "-out", cert, "-days", "3",
            "-subj", "/CN=render-capture.w3ft.net",
            "-addext", "subjectAltName=DNS:render-capture.w3ft.net",
        ],
        check=True,
        capture_output=True,
    )
    print(f"[*] generated self-signed cert in {d}", file=sys.stderr)
    return cert, key


_SNI = {}  # fd → servername, populated by the SNI callback during handshake


def _sni_cb(sock, servername, ctx):
    if servername:
        _SNI[sock.fileno()] = servername


def _handle(conn, addr, outfh, lock):
    peer = f"{addr[0]}:{addr[1]}"
    sni = _SNI.pop(conn.fileno(), None)
    meta = {
        "event": "connect",
        "ts": _ts(),
        "peer": peer,
        "sni": sni,
        "tls_version": conn.version(),
        "cipher": conn.cipher(),
        "alpn": conn.selected_alpn_protocol(),
    }
    with lock:
        outfh.write(json.dumps(meta) + "\n")
        outfh.flush()
    print(f"[+] connect {peer} tls={conn.version()} sni={sni}", file=sys.stderr)

    conn.settimeout(120)
    nchunks = 0
    nbytes = 0
    try:
        while True:
            try:
                chunk = conn.recv(65536)
            except socket.timeout:
                print(f"[.] {peer} idle 120s, still open", file=sys.stderr)
                continue
            except (ConnectionResetError, ssl.SSLError, OSError) as e:
                print(f"[!] {peer} recv error: {e}", file=sys.stderr)
                break
            if not chunk:
                break
            nchunks += 1
            nbytes += len(chunk)
            rec = {
                "event": "recv",
                "ts": _ts(),
                "peer": peer,
                "n": nchunks,
                "len": len(chunk),
                "b64": base64.b64encode(chunk).decode(),
            }
            with lock:
                outfh.write(json.dumps(rec) + "\n")
                outfh.flush()
            preview = chunk[:180].decode("utf-8", "replace").replace("\n", "\\n")
            print(f"[>] {peer} recv#{nchunks} {len(chunk)}B  {preview}", file=sys.stderr)
    finally:
        with lock:
            outfh.write(
                json.dumps(
                    {
                        "event": "close",
                        "ts": _ts(),
                        "peer": peer,
                        "chunks": nchunks,
                        "bytes": nbytes,
                    }
                )
                + "\n"
            )
            outfh.flush()
        print(f"[-] close {peer} ({nchunks} chunks, {nbytes} B)", file=sys.stderr)
        conn.close()


def serve(port, certfile, keyfile, outpath):
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(certfile, keyfile)
    # Accept whatever Render offers; we're capturing, not enforcing.
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.sni_callback = _sni_cb  # capture the SNI Render sends (auth-design input)

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("0.0.0.0", port))
    srv.listen(16)
    print(f"[*] listening on :{port} (TLS), writing {outpath}", file=sys.stderr)
    print("[*] Ctrl-C to stop and analyse", file=sys.stderr)

    # SIGTERM → behave like Ctrl-C so `kill <pid>` on a backgrounded run
    # still triggers the shutdown analysis (bash makes background jobs
    # ignore SIGINT, so we can't rely on that one).
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))

    lock = threading.Lock()
    outfh = open(outpath, "a", buffering=1)
    try:
        while True:
            raw, addr = srv.accept()
            try:
                tls = ctx.wrap_socket(raw, server_side=True)
            except ssl.SSLError as e:
                print(f"[!] TLS handshake failed from {addr[0]}: {e}", file=sys.stderr)
                # This is the signal that Render validates our cert.
                with lock:
                    outfh.write(
                        json.dumps(
                            {"event": "tls_error", "ts": _ts(), "peer": f"{addr[0]}:{addr[1]}", "err": str(e)}
                        )
                        + "\n"
                    )
                raw.close()
                continue
            threading.Thread(target=_handle, args=(tls, addr, outfh, lock), daemon=True).start()
    except KeyboardInterrupt:
        print("\n[*] stopping", file=sys.stderr)
    finally:
        outfh.close()
        srv.close()
    analyze(outpath)


# ── analysis ───────────────────────────────────────────────────────────────────

RFC5424_HEAD = re.compile(
    r"^<(?P<pri>\d{1,3})>(?P<ver>\d)\s"
    r"(?P<ts>\S+)\s(?P<host>\S+)\s(?P<app>\S+)\s(?P<procid>\S+)\s(?P<msgid>\S+)\s"
    r"(?P<rest>.*)$",
    re.DOTALL,
)


def _reassemble(path):
    """Return {peer: bytes} concatenating every recv chunk in order."""
    streams = {}
    with open(path) as fh:
        for line in fh:
            rec = json.loads(line)
            if rec.get("event") == "recv":
                streams.setdefault(rec["peer"], bytearray()).extend(base64.b64decode(rec["b64"]))
    return {k: bytes(v) for k, v in streams.items()}


def _detect_framing(buf: bytes):
    """octet-counted → b'123 <...'; non-transparent → b'<...\\n<...'."""
    m = re.match(rb"^(\d{1,6}) <", buf)
    if m:
        return "octet-counted"
    if buf.startswith(b"<"):
        return "non-transparent (LF/other delimited)"
    return "unknown"


def _split_octet(buf: bytes):
    frames, i = [], 0
    while i < len(buf):
        sp = buf.find(b" ", i)
        if sp < 0:
            break
        try:
            n = int(buf[i:sp])
        except ValueError:
            break
        frame = buf[sp + 1 : sp + 1 + n]
        frames.append(frame)
        i = sp + 1 + n
    return frames


def _split_nontransparent(buf: bytes):
    # Prefer LF; fall back to NUL. Trailing empty dropped.
    parts = buf.split(b"\n")
    return [p for p in parts if p.strip()]


def _parse_sd(rest: str):
    """Pull the STRUCTURED-DATA blob (or '-') off the front of `rest`.
    Returns (sd_string, message)."""
    rest = rest.lstrip()
    if rest.startswith("-"):
        return "-", rest[1:].lstrip()
    if not rest.startswith("["):
        return None, rest
    depth, i, out = 0, 0, []
    while i < len(rest):
        c = rest[i]
        if c == "[":
            depth += 1
        elif c == "]":
            depth -= 1
            if depth == 0:
                out.append(rest[: i + 1])
                return "".join(out), rest[i + 1 :].lstrip()
        i += 1
    return rest, ""


def analyze(path):
    print("\n" + "=" * 78)
    print(f"ANALYSIS  {path}")
    print("=" * 78)
    streams = _reassemble(path)
    if not streams:
        print("no recv chunks captured — did Render connect? check for tls_error rows.")
        # surface handshake failures
        with open(path) as fh:
            for line in fh:
                rec = json.loads(line)
                if rec.get("event") in ("tls_error", "connect", "close"):
                    print(" ", json.dumps(rec))
        return

    sd_ids = set()
    for peer, buf in streams.items():
        framing = _detect_framing(buf)
        print(f"\n── stream {peer}  ({len(buf)} B)")
        print(f"   framing: {framing}")
        print(f"   head bytes: {buf[:64]!r}")

        if framing == "octet-counted":
            frames = _split_octet(buf)
        else:
            frames = _split_nontransparent(buf)
        print(f"   frames: {len(frames)}")

        for fi, fr in enumerate(frames[:12]):
            s = fr.decode("utf-8", "replace")
            m = RFC5424_HEAD.match(s)
            print(f"\n   frame[{fi}] {len(fr)}B")
            if not m:
                print(f"     (no RFC5424 header)  {s[:200]!r}")
                continue
            sd, msg = _parse_sd(m["rest"])
            if sd and sd != "-":
                for sdid in re.findall(r"\[([\w@.\-]+)", sd):
                    sd_ids.add(sdid)
            pri = int(m["pri"])
            print(f"     PRI={pri}  facility={pri >> 3} severity={pri & 7}  VER={m['ver']}")
            print(f"     TIMESTAMP={m['ts']}")
            print(f"     HOSTNAME ={m['host']}")
            print(f"     APP-NAME ={m['app']}")
            print(f"     PROCID   ={m['procid']}")
            print(f"     MSGID    ={m['msgid']}")
            print(f"     SD       ={sd!r}")
            print(f"     MSG      ={msg[:200]!r}")

        if len(frames) > 12:
            print(f"\n   … {len(frames) - 12} more frames")

    if sd_ids:
        print(f"\n>>> STRUCTURED-DATA SD-IDs seen: {sorted(sd_ids)}")
        print(">>> (this is where a token would live — note the @<enterprise-number>)")
    else:
        print("\n>>> no STRUCTURED-DATA on any frame (all '-')")
        print(">>> → generic endpoint gets NO token; weft must auth another way (SNI).")

    # connection count / batching
    with open(path) as fh:
        events = [json.loads(l) for l in fh]
    conns = [e for e in events if e.get("event") == "connect"]
    recvs = [e for e in events if e.get("event") == "recv"]
    print(f"\n>>> connections: {len(conns)}   recv() calls: {len(recvs)}")
    if conns:
        snis = {c.get("sni") for c in conns}
        print(f">>> SNI values offered by Render: {snis}")
        print(">>> (if the SNI is our endpoint hostname, SNI-based tenant auth is viable)")


# ── main ───────────────────────────────────────────────────────────────────────


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--port", type=int, default=6514)
    ap.add_argument("--cert", help="TLS cert (PEM, fullchain). Omit for self-signed.")
    ap.add_argument("--key", help="TLS private key (PEM).")
    ap.add_argument("--out", help="capture JSONL path (default capture-<ts>.jsonl)")
    ap.add_argument("--analyze", metavar="FILE", help="analyse an existing capture and exit")
    args = ap.parse_args()

    if args.analyze:
        analyze(args.analyze)
        return

    if args.cert and args.key:
        cert, key = args.cert, args.key
    else:
        cert, key = _selfsigned()

    out = args.out or f"capture-{dt.datetime.now().strftime('%Y%m%d-%H%M%S')}.jsonl"
    serve(args.port, cert, key, out)


if __name__ == "__main__":
    main()
