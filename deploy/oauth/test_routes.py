"""Exercise the production Caddy fragment with a fake landing handler."""

import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import unittest
from urllib.error import HTTPError
from urllib.request import Request, urlopen


ROOT = Path(__file__).resolve().parent


class OAuthRoutesTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        caddy = shutil.which(os.environ.get("CADDY_BIN", "caddy"))
        if not caddy:
            raise RuntimeError("Install Caddy >= 2.8 or set CADDY_BIN to its executable")
        cls.temp = tempfile.TemporaryDirectory(prefix="innolive-oauth-test-")
        cls.addClassCleanup(cls.temp.cleanup)
        cls.work = Path(cls.temp.name)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        cls.base = f"http://127.0.0.1:{port}"
        config = cls.work / "Caddyfile"
        cls.access_log = cls.work / "access.jsonl"
        config.write_text(f"""{{
    admin off
    auto_https off
}}
{cls.base} {{
    log {{
        output file {cls.access_log}
        format json
    }}
    import {ROOT / 'innolive-oauth.caddy'}
    handle {{
        respond \"landing sentinel\" 418
    }}
}}
""")
        env = {**os.environ, "INNOLIVE_OAUTH_ASSET_ROOT": str(ROOT / "public")}
        subprocess.run([caddy, "validate", "--config", str(config)], env=env, check=True,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        output = (cls.work / "runtime.log").open("wb")
        cls.addClassCleanup(output.close)
        cls.process = subprocess.Popen([caddy, "run", "--config", str(config)], env=env,
                                       stdout=output, stderr=output)
        cls.addClassCleanup(cls.stop_server)
        for _ in range(100):
            try:
                cls.fetch("/ready")
                break
            except OSError:
                if cls.process.poll() is not None:
                    raise RuntimeError((cls.work / "runtime.log").read_text())
                time.sleep(0.05)
        else:
            raise RuntimeError("Caddy did not become ready")

    @classmethod
    def stop_server(cls):
        cls.process.terminate()
        cls.process.wait(timeout=5)

    @classmethod
    def fetch(cls, path, method="GET"):
        request = Request(cls.base + path, method=method,
                          headers={"Accept-Language": "ko", "Cookie": "NEXT_LOCALE=ko"})
        try:
            response = urlopen(request, timeout=3)
        except HTTPError as error:
            response = error
        with response:
            return response.status, response.headers, response.read()

    def test_both_aasa_paths_are_direct_json(self):
        expected = json.loads((ROOT / "public/apple-app-site-association").read_text())
        for path in ["/.well-known/apple-app-site-association", "/apple-app-site-association"]:
            with self.subTest(path=path):
                status, headers, body = self.fetch(path)
                self.assertEqual(status, 200)
                self.assertEqual(headers["Content-Type"], "application/json")
                self.assertIsNone(headers.get("Location"))
                self.assertEqual(json.loads(body), expected)

    def test_app_and_path_are_limited(self):
        _, _, body = self.fetch("/.well-known/apple-app-site-association?locale=ko")
        self.assertEqual(json.loads(body), {"applinks": {"details": [{
            "appIDs": ["SPT4X66Z4V.com.framework.innolive"],
            "components": [{"/": "/auth/chzzk/callback"}],
        }]}, "webcredentials": {
            "apps": ["SPT4X66Z4V.com.framework.innolive"],
        }})

    def test_https_authentication_service_is_present_on_both_paths(self):
        for path in ["/.well-known/apple-app-site-association", "/apple-app-site-association"]:
            with self.subTest(path=path):
                _, _, body = self.fetch(path)
                services = json.loads(body)
                self.assertEqual(services.get("webcredentials"), {
                    "apps": ["SPT4X66Z4V.com.framework.innolive"],
                })

    def test_callback_never_echoes_oauth_values(self):
        status, headers, body = self.fetch("/auth/chzzk/callback?code=secret-code&state=secret-state")
        self.assertEqual(status, 200)
        self.assertIsNone(headers.get("Location"))
        self.assertEqual(body, (ROOT / "public/chzzk-callback.html").read_bytes())
        self.assertNotIn(b"secret-code", body)
        self.assertNotIn(b"secret-state", body)
        self.assertNotIn(b"<script", body)
        self.assertEqual(headers["Cache-Control"], "no-store")
        self.assertEqual(headers["Referrer-Policy"], "no-referrer")
        self.assertIn("default-src 'none'", headers["Content-Security-Policy"])

    def test_head_has_same_security_headers(self):
        status, headers, body = self.fetch("/auth/chzzk/callback?code=head-secret", "HEAD")
        self.assertEqual(status, 200)
        self.assertEqual(body, b"")
        self.assertEqual(headers["Cache-Control"], "no-store")

    def test_unrelated_paths_still_reach_landing(self):
        for path in ["/", "/ko", "/auth/chzzk/other", "/other/apple-app-site-association"]:
            with self.subTest(path=path):
                status, _, body = self.fetch(path)
                self.assertEqual(status, 418)
                self.assertEqual(body, b"landing sentinel")

    def test_callback_query_is_absent_from_access_and_runtime_logs(self):
        marker = "oauth-log-test-secret"
        for method in ["GET", "HEAD", "POST"]:
            self.fetch(f"/auth/chzzk/callback?code={marker}&state={marker}", method)
        self.fetch("/ordinary-log-control")
        # Caddy's file writer writes entries synchronously, but wait for the control
        # entry so absence cannot accidentally pass because logging is disabled.
        for _ in range(50):
            logs = self.access_log.read_text() if self.access_log.exists() else ""
            if "/ordinary-log-control" in logs:
                break
            time.sleep(0.02)
        self.assertIn("/ordinary-log-control", logs)
        self.assertNotIn("/auth/chzzk/callback", logs)
        self.assertNotIn(marker, logs)
        self.assertNotIn(marker, (self.work / "runtime.log").read_text())


if __name__ == "__main__":
    unittest.main(verbosity=2)
