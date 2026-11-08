"""Logging HTTP/1.1 reverse proxy that injects a fixed per-request delay.

Usage: python3 -I latency_proxy.py <listen_port> <upstream_port> <delay_file> <log_file>

The delay (milliseconds) is re-read from <delay_file> on every request, so it can
be changed without restarting. Each request is logged as one JSON line with
monotonic start/end timestamps and the number of requests in flight when it
started, so request parallelism can be measured exactly.

The delay is a server-side sleep before forwarding: it models one round trip per
request on an already-open keep-alive connection. It does not model TCP/TLS
handshakes or bandwidth limits.
"""
import http.client
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LISTEN, UPSTREAM, DELAY_FILE, LOG_FILE = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4]
lock = threading.Lock()
inflight = 0
local = threading.local()
logf = open(LOG_FILE, "a", buffering=1)


def delay_s():
    try:
        with open(DELAY_FILE) as f:
            return float(f.read().strip() or 0) / 1000.0
    except OSError:
        return 0.0


def upstream():
    c = getattr(local, "conn", None)
    if c is None:
        c = http.client.HTTPConnection("127.0.0.1", UPSTREAM, timeout=600)
        local.conn = c
    return c


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            out = b""
            while True:
                size = int(self.rfile.readline().strip(), 16)
                if size == 0:
                    self.rfile.readline()
                    break
                out += self.rfile.read(size)
                self.rfile.readline()
            return out
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def _do(self):
        global inflight
        t0 = time.monotonic()
        with lock:
            inflight += 1
            inf = inflight
        body = self._body()
        time.sleep(delay_s())
        hdrs = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "transfer-encoding", "connection", "content-length")}
        if body or self.command in ("POST", "PUT"):
            hdrs["Content-Length"] = str(len(body))
        for attempt in range(2):
            try:
                c = upstream()
                c.request(self.command, self.path, body=body if body else None, headers=hdrs)
                r = c.getresponse()
                data = r.read()
                break
            except (http.client.HTTPException, OSError):
                local.conn = None
                if attempt:
                    raise
        self.send_response(r.status)
        for k, v in r.getheaders():
            if k.lower() not in ("transfer-encoding", "connection", "content-length"):
                self.send_header(k, v)
        if self.command == "HEAD":
            # HEAD carries the object's size in Content-Length; pass it through.
            self.send_header("Content-Length", r.getheader("Content-Length") or "0")
        else:
            self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(data)
        t1 = time.monotonic()
        with lock:
            inflight -= 1
        logf.write(json.dumps({"t0": t0, "t1": t1, "m": self.command, "p": self.path, "s": r.status,
                               "req": len(body), "resp": len(data), "inflight": inf}) + "\n")

    do_GET = do_HEAD = do_POST = do_PUT = do_DELETE = _do


class S(ThreadingHTTPServer):
    daemon_threads = True


if __name__ == "__main__":
    S(("127.0.0.1", LISTEN), H).serve_forever()
