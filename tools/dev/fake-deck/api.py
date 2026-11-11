"""The original deck's stream-sync API inside tools/dev/fake-deck.sh."""
import http.server
import json


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if not self.path.startswith("/v1/sc5000/ui"):
            return self.send_error(404)
        with open("/etc/sc5000/version.txt") as f:
            body = json.dumps({"version": f.read().strip(), "fakeDeck": True}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", 8080), H).serve_forever()
