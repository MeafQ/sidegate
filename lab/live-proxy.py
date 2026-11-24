# Serves the lab page on a LAN address with the stand-in login cookie added. Started by lab/live.sh.
# Usage: live-proxy.py LISTEN_HOST LISTEN_PORT UPSTREAM_PORT PAGE, where PAGE is the base path without its last slash
import http.client, http.server, socketserver, sys, urllib.parse

HOST, PORT, UP_PORT, PAGE = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
NGINX = "127.0.0.1:8080"
HOP = {"connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade", "proxy-connection", "host", "content-length"}


def on_page(target):
    # nginx decodes the path and resolves dot segments before it picks a location, so the lab's
    # other locations, such as /echo/, are reachable only through a path with a dot segment.
    path = urllib.parse.unquote(target.split("?", 1)[0])
    segments = path.split("/")
    return (path == PAGE or path.startswith(PAGE + "/")) and not {".", ".."} & set(segments)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def relay(self):
        if not on_page(self.path):
            self.send_error(404)
            return
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else None
        headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP and k.lower() != "cookie"}
        headers["Cookie"] = "; ".join(c for c in [self.headers.get("Cookie"), "UNIFI_TOKEN=lab-login"] if c)
        headers["Host"] = NGINX
        if body is not None:
            headers["Content-Length"] = str(len(body))
        up = http.client.HTTPConnection("127.0.0.1", UP_PORT, timeout=None)
        up.request(self.command, self.path, body=body, headers=headers)
        r = up.getresponse()
        self.send_response(r.status, r.reason)
        for k, v in r.getheaders():
            if k.lower() == "location":
                v = v.replace("http://" + NGINX, "http://" + (self.headers.get("Host") or f"{HOST}:{PORT}"))
            if k.lower() not in HOP:
                self.send_header(k, v)
        if self.command == "HEAD" or r.status in (204, 304) or r.status < 200:
            self.send_header("Content-Length", "0")
            self.end_headers()
            up.close()
            return
        # Chunked, so the event stream reaches the browser as it is written.
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        try:
            while chunk := r.read1(65536):
                self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
                self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True
        finally:
            up.close()

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = relay


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


try:
    Server((HOST, PORT), Handler).serve_forever()
except KeyboardInterrupt:
    pass
