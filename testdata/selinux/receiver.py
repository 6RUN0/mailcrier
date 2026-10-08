"""Receiver of the SELinux check: logs every request on stdout and answers
GET /wait?for=<text>&timeout=<seconds> with 200 once a request carrying
the text arrived since start, 504 past the timeout, so check.sh waits on
the event instead of on time."""

import http.server
import sys
import threading
import urllib.parse

seen = []
arrived = threading.Condition()


class Handler(http.server.BaseHTTPRequestHandler):
    """Records POST and PUT requests, serves the wait."""

    def record(self):
        size = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(size).decode("utf-8", "replace")
        print(self.command, self.path, body[:400], flush=True)
        with arrived:
            seen.append(self.path + "\n" + str(self.headers) + body)
            arrived.notify_all()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")

    do_POST = do_PUT = record

    def do_GET(self):
        url = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(url.query)
        if url.path != "/wait" or "for" not in query:
            self.send_error(404)
            return
        text = query["for"][0]
        timeout = float(query.get("timeout", ["60"])[0])
        with arrived:
            found = arrived.wait_for(lambda: any(text in s for s in seen), timeout)
        self.send_response(200 if found else 504)
        self.end_headers()

    def log_message(self, format, *args):
        pass


def main():
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 80), Handler)
    print("receiver listening", flush=True)
    sys.stdout.flush()
    server.serve_forever()


if __name__ == "__main__":
    main()
