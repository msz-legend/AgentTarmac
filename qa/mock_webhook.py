# qa/mock_webhook.py - local webhook receiver standing in for Feishu / WeCom
# group bots (TC-F9). Logs every POST (path + body + arrival timestamp) to
# qa/out/webhook.log so the QA scripts can assert delivery and measure latency.
# /feishu*  -> {"code": 0}        (Feishu custom-bot success shape)
# /wecom*   -> {"errcode": 0}     (WeCom group-bot success shape)
import datetime
import http.server
import json

LOG = r"D:/DevTools/AgentHub/qa/out/webhook.log"


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n).decode("utf-8", "replace")
        ts = datetime.datetime.now().isoformat(timespec="milliseconds")
        with open(LOG, "a", encoding="utf-8") as f:
            f.write(f"{ts} {self.path} {body}\n")
        if "feishu" in self.path:
            resp = {"code": 0}
        else:
            resp = {"errcode": 0, "errmsg": "ok"}
        raw = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    print("mock webhook listening on 127.0.0.1:18900")
    http.server.HTTPServer(("127.0.0.1", 18900), Handler).serve_forever()
