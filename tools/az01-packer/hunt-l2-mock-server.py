import http.server
import json
import os
import sys

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        p = self.path.split('?', 1)[0]
        if p == '/v1/enginex/ui':
            res = {
                "version": "5.3.0.5",
                "consoleUrl": "http://192.168.1.50:8080",
                "accessCode": "123456",
                "updateRepo": "Cd1s/EngineX-OS",
                "fourDeckWaveform": True,
                "fourDeckRole": "12",
                "fourDeckRoleActive": "12",
                "sources": ["tidal", "netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"],
                "bpmDecimals": 2,
                "usbPreparing": False,
                "sdHighSpeed": False
            }
            body = json.dumps(res).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        elif p == '/v1/enginex/update':
            res = {
                "currentVersion": "5.3.0.5",
                "latestVersion": "v5.3.0.6",
                "updateAvailable": True,
                "downloadUrl": "http://127.0.0.1:8080/update.img"
            }
            body = json.dumps(res).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        elif p == '/v1/enginex/console-qr':
            # 16x16 PNG
            png = bytes.fromhex('89504e470d0a1a0a0000000d49484452000000100000001008060000001ff3ff61000000017352474200aece1ce90000001b49444154384f63606060f8cfc0c0c00003301828000000ffff0300138902d7367852f40000000049454e44ae426082')
            self.send_response(200)
            self.send_header('Content-Type', 'image/png')
            self.send_header('Content-Length', str(len(png)))
            self.end_headers()
            self.wfile.write(png)
            return
        elif p == '/v1/enginex/clock':
            res = {"bpm": 128.0, "source": "master", "deck": 1}
            body = json.dumps(res).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        elif p == '/v1/enginex/views':
            res = {"active": 1, "views": {}}
            body = json.dumps(res).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', '2')
        self.end_headers()
        self.wfile.write(b'{}')

    def do_POST(self):
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', '11')
        self.end_headers()
        self.wfile.write(b'{"ok":true}')

port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
srv = http.server.ThreadingHTTPServer(('127.0.0.1', port), H)
srv.serve_forever()
