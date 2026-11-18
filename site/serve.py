#!/usr/bin/env python3
"""Serve site/ and proxy /v1/* to the gateway, so the playground page is
same-origin. The gateway answers /v1/chat/completions without CORS headers, so a
page opened from another origin cannot read the reply; proxying keeps the browser
call same-origin and leaves the gateway untouched.

    python site/serve.py            # http://127.0.0.1:8088/antigravity.html
    GATEWAY=http://127.0.0.1:20130 PORT=8088 python site/serve.py
"""

import http.client
import http.server
import os
import sys
import urllib.parse

GATEWAY = os.environ.get("GATEWAY", "http://127.0.0.1:20130")
PORT = int(os.environ.get("PORT", "8088"))
ROOT = os.path.dirname(os.path.abspath(__file__))

_gw = urllib.parse.urlsplit(GATEWAY)
GW_HOST, GW_PORT = _gw.hostname, _gw.port or 80


class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=ROOT, **kwargs)

    def _proxy(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else None
        forwarded = {
            k: v
            for k, v in self.headers.items()
            if k.lower() not in ("host", "content-length", "accept-encoding", "connection")
        }
        conn = http.client.HTTPConnection(GW_HOST, GW_PORT, timeout=600)
        try:
            conn.request(self.command, self.path, body=body, headers=forwarded)
            resp = conn.getresponse()
            self.send_response(resp.status)
            for k, v in resp.getheaders():
                if k.lower() in ("transfer-encoding", "connection", "content-length"):
                    continue
                self.send_header(k, v)
            self.end_headers()
            # Stream as it arrives: an SSE answer is useless buffered, and HTTP/1.0
            # close-delimited framing is what SimpleHTTPRequestHandler speaks.
            while True:
                chunk = resp.read1(8192) if hasattr(resp, "read1") else resp.read(8192)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
        except (OSError, http.client.HTTPException) as exc:
            if not self.headers_sent:
                self.send_error(502, f"gateway unreachable: {exc}")
        finally:
            conn.close()

    def do_POST(self):
        self._proxy()

    def do_DELETE(self):
        self._proxy()

    def do_PUT(self):
        self._proxy()


if __name__ == "__main__":
    with http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler) as srv:
        print(f"serving {ROOT} on http://127.0.0.1:{PORT}, /v1/* -> {GATEWAY}", flush=True)
        try:
            srv.serve_forever()
        except KeyboardInterrupt:
            sys.exit(0)