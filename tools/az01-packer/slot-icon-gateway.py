# Sandbox only (slot-icon-sandbox.sh): answers the pack's lookup like the gateway does for tracks it has no stemmed copy of: 404, with the
# service named in X-EngineX-Provider (Go sends the canonical spelling below). Ids 1-7 are tracks of seven services; any other id is unknown.
import http.server

SERVICES = {"1": "deezer", "2": "soundcloud", "3": "beatport", "4": "rekordbox", "5": "spotify", "6": "applemusic", "7": "netease"}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(404)
        service = SERVICES.get(self.path.rsplit("id=", 1)[-1])
        if service:
            self.send_header("X-Enginex-Provider", service)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", 8080), Handler).serve_forever()
