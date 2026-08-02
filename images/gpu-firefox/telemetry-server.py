#!/usr/bin/python3
import json
import os
import re
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

ROOT = "/usr/share/vmsh-webgl"
TELEMETRY = "/shared/firefox-webgl-telemetry.json"


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=ROOT, **kwargs)

    def log_message(self, message, *args):
        print(message % args, flush=True)

    def do_POST(self):
        capture = re.fullmatch(r"/capture/([0-3])", self.path)
        if self.path != "/telemetry" and capture is None:
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self.send_error(400)
            return
        maximum = 8 * 1024 * 1024 if capture is not None else 65536
        if length <= 0 or length > maximum:
            self.send_error(400)
            return
        body = self.rfile.read(length)
        if capture is not None:
            if not body.startswith(b"\x89PNG\r\n\x1a\n"):
                self.send_error(400)
                return
            output_path = f"/shared/firefox-webgl-scene-{capture.group(1)}.png"
            temporary = output_path + ".new"
            with open(temporary, "wb") as output:
                output.write(body)
            os.replace(temporary, output_path)
            self.send_response(204)
            self.end_headers()
            return
        try:
            payload = json.loads(body)
        except (UnicodeDecodeError, json.JSONDecodeError):
            self.send_error(400)
            return
        temporary = TELEMETRY + ".new"
        with open(temporary, "w", encoding="utf-8") as output:
            json.dump(payload, output, indent=2, sort_keys=True)
            output.write("\n")
        os.replace(temporary, TELEMETRY)
        self.send_response(204)
        self.end_headers()


ThreadingHTTPServer(("127.0.0.1", 8080), Handler).serve_forever()
