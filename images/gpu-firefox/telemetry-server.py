#!/usr/bin/python3
import json
import os
import re
from urllib.parse import parse_qs, urlparse
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

ROOT = "/usr/share/vmsh-webgl"
TELEMETRY = "/shared/firefox-webgl-telemetry.json"
CTS_RESULT_PREFIX = "/shared/firefox-webgl-cts"


def atomic_write(path, body):
    temporary = path + ".new"
    with open(temporary, "wb") as output:
        output.write(body)
    os.replace(temporary, path)


def cts_result_stem(referer):
    version = parse_qs(urlparse(referer).query).get("version", [""])[0]
    if version.startswith("1."):
        return CTS_RESULT_PREFIX + "-webgl1"
    if version.startswith("2."):
        return CTS_RESULT_PREFIX + "-webgl2"
    return CTS_RESULT_PREFIX + "-unknown"


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=ROOT, **kwargs)

    def log_message(self, message, *args):
        print(message % args, flush=True)

    def do_POST(self):
        capture = re.fullmatch(r"/capture/([0-3])", self.path)
        is_cts_start = self.path == "/start"
        is_cts_finish = self.path == "/finish"
        if self.path != "/telemetry" and capture is None and not is_cts_start and not is_cts_finish:
            self.send_error(404)
            return
        if is_cts_start:
            stem = cts_result_stem(self.headers.get("Referer", ""))
            atomic_write(stem + ".status", b"running\n")
            self.send_response(204)
            self.end_headers()
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self.send_error(400)
            return
        if is_cts_finish:
            maximum = 64 * 1024 * 1024
        elif capture is not None:
            maximum = 8 * 1024 * 1024
        else:
            maximum = 65536
        if length <= 0 or length > maximum:
            self.send_error(400)
            return
        body = self.rfile.read(length)
        if is_cts_finish:
            stem = cts_result_stem(self.headers.get("Referer", ""))
            atomic_write(stem + ".txt", body)
            atomic_write(stem + ".status", b"complete\n")
            self.send_response(204)
            self.end_headers()
            return
        if capture is not None:
            if not body.startswith(b"\x89PNG\r\n\x1a\n"):
                self.send_error(400)
                return
            output_path = f"/shared/firefox-webgl-scene-{capture.group(1)}.png"
            atomic_write(output_path, body)
            self.send_response(204)
            self.end_headers()
            return
        try:
            payload = json.loads(body)
        except (UnicodeDecodeError, json.JSONDecodeError):
            self.send_error(400)
            return
        encoded = (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode()
        atomic_write(TELEMETRY, encoded)
        self.send_response(204)
        self.end_headers()


def main():
    ThreadingHTTPServer(("127.0.0.1", 8080), Handler).serve_forever()


if __name__ == "__main__":
    main()
