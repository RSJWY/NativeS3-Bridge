#!/usr/bin/env python3
"""Small dependency-free Panel browser gate.

The release workflow already provides Chrome and ChromeDriver.  Keeping this
client here (instead of adding a JavaScript browser framework) makes the check
portable to a developer machine and keeps the browser profile disposable.
No response body, cookie, password, or signed URL is printed by this module.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any, Iterable
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit, urlunsplit
from urllib.request import Request, urlopen


DEFAULT_TIMEOUT = 30.0
DRIVER_POLL_INTERVAL = 0.1
ELEMENT_POLL_INTERVAL = 0.2
# Standalone-only dashboard subpaths stay forbidden; the Panel node-health
# summary now legitimately lives at /api/admin/dashboard/summary.
FORBIDDEN_API_PREFIXES = (
    "/api/admin/dashboard/usage-ranking",
    "/api/admin/dashboard/request-trend",
    "/api/admin/credentials",
    "/api/admin/buckets",
)
EXPECTED_API_RESPONSES = (
    # No pending in-place import is the documented idle state. The typed API
    # client normalizes this exact GET response to null on Node-detail mount.
    (404, re.compile(r"^/api/admin/nodes/\d+/import$")),
)
ELEMENT_KEY = "element-6066-11e4-a52e-4f735466cecf"
LEGACY_ELEMENT_KEY = "ELEMENT"


class BrowserGateError(RuntimeError):
    """A redacted, user-facing browser gate failure."""


def _parse_args() -> argparse.Namespace:
    webdriver_home = os.environ.get("CHROMEWEBDRIVER", "")
    default_driver = os.environ.get("CHROMEDRIVER")
    if not default_driver and webdriver_home:
        default_driver = str(Path(webdriver_home) / "chromedriver")
    parser = argparse.ArgumentParser(description="Run the Panel ChromeDriver E2E gate")
    parser.add_argument(
        "--panel-url",
        required=True,
        help="Panel admin base URL, for example http://127.0.0.1:9001",
    )
    parser.add_argument(
        "--admin-password",
        default=None,
        help="Temporary admin password (prefer E2E_ADMIN_PASSWORD to avoid argv exposure)",
    )
    parser.add_argument(
        "--expected-node-name",
        required=True,
        help="Node display name that must be visible in the Panel node list",
    )
    parser.add_argument(
        "--chromedriver",
        default=default_driver,
        help="ChromeDriver executable or command (auto-detected when omitted)",
    )
    parser.add_argument(
        "--chrome",
        default=os.environ.get("CHROME_BIN") or os.environ.get("GOOGLE_CHROME_BIN"),
        help="Chrome/Chromium executable (auto-detected when omitted)",
    )
    parser.add_argument(
        "--report",
        default=None,
        help="Optional path for a redacted diagnostic report",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=DEFAULT_TIMEOUT,
        help="Per-step timeout in seconds (default: 30)",
    )
    args = parser.parse_args()
    if args.admin_password is None:
        args.admin_password = os.environ.get("E2E_ADMIN_PASSWORD", "")
    if not args.admin_password:
        parser.error("--admin-password or E2E_ADMIN_PASSWORD is required")
    if not args.expected_node_name.strip():
        parser.error("--expected-node-name must not be empty")
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    return args


def _normalise_panel_url(raw: str) -> str:
    parsed = urlsplit(raw.strip())
    if parsed.scheme not in ("http", "https") or not parsed.netloc:
        raise BrowserGateError("panel URL must use http:// or https:// with a host")
    if parsed.username is not None or parsed.password is not None:
        raise BrowserGateError("panel URL must not contain credentials")
    # The browser check must never receive a query/fragment carrying a secret.
    return urlunsplit((parsed.scheme, parsed.netloc, parsed.path.rstrip("/"), "", ""))


def _find_executable(value: str | None, candidates: Iterable[str]) -> str:
    if value:
        parts = shlex.split(value)
        if not parts:
            raise BrowserGateError("browser command is empty")
        command = parts[0]
        if os.path.sep in command:
            if not (os.path.isfile(command) and os.access(command, os.X_OK)):
                raise BrowserGateError(f"browser executable is not runnable: {Path(command).name}")
            return value
        if shutil.which(command):
            return value
        raise BrowserGateError(f"browser executable was not found: {command}")
    for candidate in candidates:
        path = shutil.which(candidate)
        if path:
            return path
    raise BrowserGateError("Chrome executable was not found")


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _redact(text: str, secrets: Iterable[str] = ()) -> str:
    result = str(text)
    for secret in secrets:
        if secret:
            result = result.replace(secret, "[REDACTED]")
    # Keep diagnostics useful while preventing accidental credential/cookie
    # leakage from ChromeDriver or application error text.
    result = re.sub(r"(?i)(password|secret|token|cookie|authorization)\s*[:=]\s*[^\s,;]+", r"\1=[REDACTED]", result)
    result = re.sub(r"(?i)AWS4-HMAC-SHA256[^\s\"']*", "AWS4-HMAC-SHA256[REDACTED]", result)
    result = re.sub(
        r"(?i)([?&][^=]*(?:x-amz-|signature|token|secret|password|cookie|authorization)[^=]*)=[^&\s]*",
        r"\1=[REDACTED]",
        result,
    )
    return result


def _safe_url(raw: str) -> str:
    try:
        parsed = urlsplit(raw)
        return urlunsplit((parsed.scheme, parsed.netloc, parsed.path, "", ""))
    except Exception:
        return "[invalid-url]"


def _page_summary(value: Any, secrets: Iterable[str]) -> str:
    if not isinstance(value, str):
        return ""
    compact = " ".join(value.split())
    if len(compact) > 600:
        compact = compact[:600] + "…"
    return _redact(compact, secrets)


class WebDriver:
    """Minimal W3C WebDriver HTTP client using urllib only."""

    def __init__(self, base_url: str, timeout: float, secrets: Iterable[str]):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.secrets = tuple(secrets)
        self.session_id: str | None = None

    def _request(self, method: str, path: str, payload: Any = None) -> Any:
        body = None
        headers = {"Accept": "application/json"}
        if payload is not None:
            body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
            headers["Content-Type"] = "application/json; charset=utf-8"
        request = Request(self.base_url + path, data=body, headers=headers, method=method)
        try:
            with urlopen(request, timeout=self.timeout) as response:
                raw = response.read()
                status = response.status
        except HTTPError as exc:
            raw = exc.read()
            status = exc.code
        except (URLError, TimeoutError, OSError) as exc:
            raise BrowserGateError(f"ChromeDriver {method} {path} failed: {_redact(str(exc), self.secrets)}") from exc
        try:
            decoded = json.loads(raw.decode("utf-8")) if raw else None
        except (UnicodeDecodeError, json.JSONDecodeError):
            decoded = None
        if status < 200 or status >= 300:
            detail = ""
            if isinstance(decoded, dict):
                value = decoded.get("value")
                if isinstance(value, dict):
                    detail = str(value.get("message", ""))
                elif isinstance(value, str):
                    detail = value
            raise BrowserGateError(
                f"ChromeDriver {method} {path} returned HTTP {status}"
                + (f": {_redact(detail, self.secrets)}" if detail else "")
            )
        if not isinstance(decoded, dict):
            return decoded
        value = decoded.get("value")
        # The W3C new-session response carries sessionId beside value, while
        # older JSON Wire responses may place it inside value.  Preserve it for
        # start() even though normal commands only need the value member.
        if path == "/session" and decoded.get("sessionId") and isinstance(value, dict):
            value = dict(value)
            value["sessionId"] = decoded["sessionId"]
        if isinstance(value, dict) and value.get("error"):
            message = _redact(str(value.get("message", value.get("error"))), self.secrets)
            raise BrowserGateError(f"ChromeDriver command failed: {message}")
        return value

    def start(self, chrome: str, profile: str) -> None:
        options: dict[str, Any] = {
            "args": [
                "--headless=new",
                "--no-sandbox",
                "--disable-dev-shm-usage",
                "--disable-gpu",
                "--window-size=1440,1000",
                "--remote-allow-origins=*",
                f"--user-data-dir={profile}",
            ],
            "excludeSwitches": ["enable-logging"],
            "prefs": {"profile.default_content_setting_values.notifications": 2},
            "binary": chrome,
            "w3c": True,
        }
        # ChromeDriver accepts this capability on current runner images.  If an
        # older driver rejects it, the caller retries a plain W3C capability.
        capabilities = {
            "browserName": "chrome",
            "goog:chromeOptions": options,
            "goog:loggingPrefs": {"performance": "ALL", "browser": "ALL"},
        }
        try:
            result = self._request("POST", "/session", {"capabilities": {"alwaysMatch": capabilities}})
        except BrowserGateError as first_error:
            if "loggingPrefs" not in str(first_error):
                raise
            capabilities.pop("goog:loggingPrefs", None)
            result = self._request("POST", "/session", {"capabilities": {"alwaysMatch": capabilities}})
        if not isinstance(result, dict):
            raise BrowserGateError("ChromeDriver did not return a session")
        self.session_id = str(result.get("sessionId") or result.get("session_id") or "")
        # Some older drivers put sessionId beside value.  Fetching it is not
        # possible through _request's value-only return, so use the endpoint's
        # standard response shape by accepting the W3C value.sessionId too.
        if not self.session_id:
            self.session_id = str(result.get("value", {}).get("sessionId", ""))
        if not self.session_id:
            raise BrowserGateError("ChromeDriver returned an empty session id")

    def stop(self) -> None:
        if not self.session_id:
            return
        session = self.session_id
        self.session_id = None
        try:
            self._request("DELETE", f"/session/{session}")
        except Exception:
            # Cleanup is best-effort; the parent process still terminates the
            # driver and removes the profile in its finally block.
            pass

    def _session_path(self, suffix: str) -> str:
        if not self.session_id:
            raise BrowserGateError("ChromeDriver session is not active")
        return f"/session/{self.session_id}{suffix}"

    def navigate(self, url: str) -> None:
        self._request("POST", self._session_path("/url"), {"url": url})

    def push_route(self, path: str) -> None:
        if not path.startswith("/") or "?" in path or "#" in path:
            raise BrowserGateError("browser route must be a clean same-origin path")
        self.execute(
            """
            window.history.pushState({}, '', arguments[0]);
            window.dispatchEvent(new PopStateEvent('popstate'));
            """,
            [path],
        )

    def current_url(self) -> str:
        value = self._request("GET", self._session_path("/url"))
        return str(value or "")

    def execute(self, script: str, args: list[Any] | None = None) -> Any:
        return self._request("POST", self._session_path("/execute/sync"), {"script": script, "args": args or []})

    def find(self, using: str, value: str) -> str:
        result = self._request("POST", self._session_path("/element"), {"using": using, "value": value})
        if not isinstance(result, dict):
            raise BrowserGateError("ChromeDriver returned an invalid element")
        return str(result.get(ELEMENT_KEY) or result.get(LEGACY_ELEMENT_KEY) or "")

    def element_value(self, element: str, value: str) -> None:
        self._request(
            "POST",
            self._session_path(f"/element/{element}/value"),
            {"text": value, "value": list(value)},
        )

    def click(self, element: str) -> None:
        self._request("POST", self._session_path(f"/element/{element}/click"), {})

    def logs(self, log_type: str = "performance") -> list[Any]:
        try:
            value = self._request("POST", self._session_path("/log"), {"type": log_type})
        except BrowserGateError:
            return []
        return value if isinstance(value, list) else []

    def resource_urls(self) -> list[str]:
        """Return same-page resource URLs when performance logs are disabled."""
        value = self.execute(
            """
            return Array.from(performance.getEntriesByType('resource'))
              .map(entry => entry && entry.name)
              .filter(name => typeof name === 'string');
            """
        )
        if not isinstance(value, list):
            return []
        return [str(item) for item in value if isinstance(item, str)]


def _wait_until(deadline: float, callback, description: str):
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            value = callback()
            if value:
                return value
        except Exception as exc:  # WebDriver returns 404 while the SPA mounts.
            last_error = exc
        time.sleep(ELEMENT_POLL_INTERVAL)
    if last_error:
        raise BrowserGateError(f"timed out waiting for {description}: {_redact(str(last_error))}") from last_error
    raise BrowserGateError(f"timed out waiting for {description}")


def _wait_driver(driver_url: str, timeout: float) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            request = Request(driver_url + "/status", headers={"Accept": "application/json"})
            with urlopen(request, timeout=min(timeout, 2.0)) as response:
                if 200 <= response.status < 300:
                    return
        except (HTTPError, URLError, TimeoutError, OSError):
            pass
        time.sleep(DRIVER_POLL_INTERVAL)
    raise BrowserGateError("timed out waiting for ChromeDriver")


def _extract_network_events(entries: Iterable[Any]) -> tuple[list[tuple[str, int]], list[str]]:
    responses: list[tuple[str, int]] = []
    requests: list[str] = []
    for entry in entries:
        raw = entry.get("message") if isinstance(entry, dict) else None
        if not isinstance(raw, str):
            continue
        try:
            outer = json.loads(raw)
            message = outer.get("message", {})
            method = message.get("method")
            params = message.get("params", {})
            if method == "Network.responseReceived":
                response = params.get("response", {})
                url = str(response.get("url", ""))
                status = int(response.get("status", 0))
                if url:
                    responses.append((url, status))
            elif method == "Network.requestWillBeSent":
                request = params.get("request", {})
                url = str(request.get("url", ""))
                if url:
                    requests.append(url)
        except (TypeError, ValueError, json.JSONDecodeError):
            continue
    return responses, requests


def _api_path(url: str) -> str:
    try:
        return urlsplit(url).path
    except Exception:
        return ""


def _origin(url: str) -> tuple[str, str]:
    try:
        parsed = urlsplit(url)
        return parsed.scheme.lower(), parsed.netloc.lower()
    except Exception:
        return "", ""


def _is_expected_api_response(status: int, path: str) -> bool:
    return any(status == expected_status and pattern.fullmatch(path) for expected_status, pattern in EXPECTED_API_RESPONSES)


def _assert_network(entries: list[Any], secrets: Iterable[str], panel_url: str | None = None) -> list[str]:
    responses, requests = _extract_network_events(entries)
    failures: list[str] = []
    expected_origin = _origin(panel_url) if panel_url else None
    for url, status in responses:
        path = _api_path(url)
        if not path.startswith("/api/"):
            continue
        if expected_origin and _origin(url) != expected_origin:
            failures.append(f"API response used a non-Panel origin {path}")
        if status >= 400 and not _is_expected_api_response(status, path):
            failures.append(f"API HTTP {status} {path}")
        if any(path.startswith(prefix) for prefix in FORBIDDEN_API_PREFIXES):
            failures.append(f"forbidden standalone API path {path}")
    for url in requests:
        path = _api_path(url)
        if path.startswith("/api/") and expected_origin and _origin(url) != expected_origin:
            failures.append(f"API request used a non-Panel origin {path}")
        if any(path.startswith(prefix) for prefix in FORBIDDEN_API_PREFIXES):
            failures.append(f"forbidden standalone API request {path}")
    # Keep the report deterministic and small if Chrome emits duplicate events.
    return list(dict.fromkeys(_redact(item, secrets) for item in failures))


def _assert_resource_urls(urls: Iterable[str], secrets: Iterable[str], panel_url: str) -> list[str]:
    """Apply route/origin checks to the browser Performance API fallback."""
    failures: list[str] = []
    expected_origin = _origin(panel_url)
    for url in urls:
        path = _api_path(url)
        if not path.startswith("/api/"):
            continue
        if _origin(url) != expected_origin:
            failures.append(f"API request used a non-Panel origin {path}")
        if any(path.startswith(prefix) for prefix in FORBIDDEN_API_PREFIXES):
            failures.append(f"forbidden standalone API request {path}")
    return list(dict.fromkeys(_redact(item, secrets) for item in failures))


def _saw_same_origin_api_path(
    entries: Iterable[Any],
    resource_urls: Iterable[str],
    panel_url: str,
    expected_path: str,
) -> bool:
    """Return whether network evidence contains the expected Panel API path."""
    responses, requests = _extract_network_events(entries)
    expected_origin = _origin(panel_url)
    urls = [url for url, _status in responses]
    urls.extend(requests)
    urls.extend(str(url) for url in resource_urls)
    return any(
        _origin(url) == expected_origin and _api_path(url).rstrip("/") == expected_path
        for url in urls
    )


def run_gate(args: argparse.Namespace) -> dict[str, Any]:
    panel_url = _normalise_panel_url(args.panel_url)
    chrome = _find_executable(args.chrome, ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"))
    driver_command = _find_executable(
        args.chromedriver,
        (
            "chromedriver",
            "chromedriver-linux64/chromedriver",
            "/usr/bin/chromedriver",
            "/usr/local/share/chromedriver-linux64/chromedriver",
        ),
    )
    driver_parts = shlex.split(driver_command)
    if not driver_parts:
        raise BrowserGateError("ChromeDriver command is empty")

    profile = tempfile.mkdtemp(prefix="natives3-e2e-chrome-")
    driver_log = Path(profile) / "chromedriver.log"
    port = _free_port()
    process: subprocess.Popen[bytes] | None = None
    webdriver: WebDriver | None = None
    report: dict[str, Any] = {"panel_url": panel_url, "checks": []}
    try:
        with driver_log.open("wb") as log_handle:
            process = subprocess.Popen(
                driver_parts + [f"--port={port}", "--log-level=SEVERE"],
                stdout=log_handle,
                stderr=subprocess.STDOUT,
                stdin=subprocess.DEVNULL,
            )
        driver_url = f"http://127.0.0.1:{port}"
        _wait_driver(driver_url, args.timeout)
        webdriver = WebDriver(driver_url, args.timeout, (args.admin_password,))
        webdriver.start(chrome, profile)
        deadline = time.monotonic() + args.timeout

        webdriver.navigate(panel_url + "/login")
        password_element = _wait_until(
            deadline,
            lambda: webdriver.find("css selector", "#password"),
            "Panel login form",
        )
        webdriver.element_value(password_element, args.admin_password)
        _wait_until(
            deadline,
            lambda: webdriver.execute(
                """
                const button = document.querySelector("button[type='submit']");
                return Boolean(button && !button.disabled);
                """
            ),
            "Panel login form readiness",
        )
        submit_element = _wait_until(
            deadline,
            lambda: webdriver.find("css selector", "button[type='submit']"),
            "Panel login button",
        )
        webdriver.click(submit_element)

        _wait_until(
            time.monotonic() + args.timeout,
            lambda: webdriver.current_url().rstrip("/").endswith("/panel-dashboard"),
            "Panel login redirect to panel dashboard",
        )
        _wait_until(
            time.monotonic() + args.timeout,
            lambda: "需要关注的节点" in str(webdriver.execute("return document.body ? document.body.innerText : '';")),
            "Panel dashboard content",
        )
        # Explicitly request the standalone dashboard route.  Panel mode must
        # redirect it back to the node list rather than loading standalone data.
        # Use the SPA's own history boundary after login.  A full WebDriver
        # navigation would recreate the app and can race its persisted session
        # bootstrap; the route guard itself is what this assertion targets.
        webdriver.push_route("/dashboard")
        current = _wait_until(
            time.monotonic() + args.timeout,
            lambda: webdriver.current_url() if webdriver.current_url().rstrip("/").endswith("/panel-dashboard") else "",
            "Panel /dashboard -> /panel-dashboard route guard",
        )
        resolved_path = _api_path(str(current)).rstrip("/") or "/"
        if resolved_path != "/panel-dashboard":
            raise BrowserGateError(f"Panel standalone dashboard route did not resolve to /panel-dashboard (got {_safe_url(str(current))})")
        webdriver.push_route("/nodes")
        _wait_until(
            time.monotonic() + args.timeout,
            lambda: webdriver.current_url().rstrip("/").endswith("/nodes"),
            "Panel navigation to node list",
        )
        expected_name = args.expected_node_name
        _wait_until(
            time.monotonic() + args.timeout,
            lambda: expected_name in str(webdriver.execute("return document.body ? document.body.innerText : '';")),
            "created node visible in Panel",
        )
        webdriver.push_route("/nodes/1")
        _wait_until(
            time.monotonic() + args.timeout,
            lambda: webdriver.current_url().rstrip("/").endswith("/nodes/1"),
            "Panel node detail route",
        )
        keyword_element = _wait_until(
            time.monotonic() + args.timeout,
            lambda: webdriver.find("css selector", "#node-log-keyword"),
            "Node log query form",
        )
        webdriver.element_value(keyword_element, "s3 request")
        _wait_until(
            time.monotonic() + args.timeout,
            lambda: webdriver.execute(
                """
                const button = Array.from(document.querySelectorAll('button'))
                  .find((candidate) => candidate.textContent.trim() === '拉取日志');
                if (!button || button.disabled) return false;
                button.click();
                return true;
                """
            ),
            "Node log query action",
        )
        _wait_until(
            time.monotonic() + args.timeout,
            lambda: "已从远程 ring 返回" in str(
                webdriver.execute("return document.body ? document.body.innerText : '';")
            ),
            "Node log query result",
        )
        # Logs are a shared authenticated route.  Visit it explicitly so the
        # Panel gate proves that the mode-specific navigation and API wiring
        # agree, while the forbidden list above continues to protect only
        # standalone-only surfaces.
        webdriver.push_route("/logs")
        _wait_until(
            time.monotonic() + args.timeout,
            lambda: webdriver.current_url().rstrip("/").endswith("/logs"),
            "Panel navigation to logs",
        )
        _wait_until(
            time.monotonic() + args.timeout,
            lambda: webdriver.execute(
                """
                const file = document.querySelector('#log-file');
                const body = document.body ? document.body.innerText : '';
                return Boolean(file && !body.includes('加载中…'));
                """
            ),
            "Panel logs page",
        )
        entries = webdriver.logs("performance")
        resource_urls = webdriver.resource_urls()
        if not entries and not resource_urls:
            raise BrowserGateError("ChromeDriver returned no network evidence")
        if not _saw_same_origin_api_path(entries, resource_urls, panel_url, "/api/admin/dashboard/summary"):
            raise BrowserGateError("no same-origin /api/admin/dashboard/summary request was observed")
        if not _saw_same_origin_api_path(entries, resource_urls, panel_url, "/api/admin/nodes"):
            raise BrowserGateError("no same-origin /api/admin/nodes request was observed")
        if not _saw_same_origin_api_path(entries, resource_urls, panel_url, "/api/admin/nodes/1/tasks"):
            raise BrowserGateError("no same-origin Node task dispatch request was observed")
        if not _saw_same_origin_api_path(entries, resource_urls, panel_url, "/api/admin/logs"):
            raise BrowserGateError("no same-origin /api/admin/logs request was observed")
        failures = _assert_network(entries, (args.admin_password, expected_name), panel_url)
        failures.extend(_assert_resource_urls(resource_urls, (args.admin_password, expected_name), panel_url))
        failures = list(dict.fromkeys(failures))
        if failures:
            raise BrowserGateError("; ".join(failures))
        report["checks"] = [
            "login",
            "dashboard_redirect",
            "panel_dashboard_content",
            "panel_dashboard_summary_api",
            "node_visible",
            "panel_nodes_api",
            "node_logs_ui",
            "node_tasks_api",
            "panel_logs_route",
            "panel_logs_api",
            "network_contract",
        ]
        report["route"] = "/logs"
        report["network_events"] = len(entries)
        return report
    except BrowserGateError as exc:
        report["error"] = _redact(str(exc), (args.admin_password, args.expected_node_name))
        if webdriver is not None:
            try:
                report["url"] = _safe_url(webdriver.current_url())
                report["page"] = _page_summary(webdriver.execute("return document.body ? document.body.innerText : '';"), (args.admin_password,))
                report["network_failures"] = _assert_network(
                    webdriver.logs("performance"),
                    (args.admin_password, args.expected_node_name),
                    panel_url,
                )
                report["network_failures"].extend(
                    _assert_resource_urls(
                        webdriver.resource_urls(),
                        (args.admin_password, args.expected_node_name),
                        panel_url,
                    )
                )
            except Exception as diagnostic_error:
                report["diagnostic_error"] = _redact(str(diagnostic_error), (args.admin_password,))
        if driver_log.exists():
            try:
                tail = driver_log.read_text(errors="replace").splitlines()[-20:]
                report["driver_log"] = _redact("\n".join(tail), (args.admin_password, args.expected_node_name))
            except OSError:
                pass
        # Preserve the redacted evidence across the exception boundary so main
        # can write it after the browser process/profile have been cleaned up.
        exc.report = report  # type: ignore[attr-defined]
        raise
    finally:
        if webdriver is not None:
            webdriver.stop()
        if process is not None:
            try:
                process.terminate()
                process.wait(timeout=5)
            except (subprocess.TimeoutExpired, OSError):
                try:
                    process.kill()
                    process.wait(timeout=2)
                except (OSError, subprocess.TimeoutExpired):
                    pass
        shutil.rmtree(profile, ignore_errors=True)


def _write_report(path: str | None, report: dict[str, Any], secrets: Iterable[str]) -> None:
    if not path:
        return
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    content = json.dumps(report, ensure_ascii=False, indent=2)
    content = _redact(content, secrets)
    target.write_text(content + "\n", encoding="utf-8")
    try:
        target.chmod(0o600)
    except OSError:
        pass


def main() -> int:
    args = _parse_args()
    report: dict[str, Any] = {}
    try:
        report = run_gate(args)
    except BrowserGateError as exc:
        failure_report = getattr(exc, "report", None)
        if isinstance(failure_report, dict):
            report = failure_report
        report.setdefault("error", _redact(str(exc), (args.admin_password, args.expected_node_name)))
        _write_report(args.report, report, (args.admin_password, args.expected_node_name))
        print(f"browser E2E failed: {_redact(str(exc), (args.admin_password, args.expected_node_name))}", file=sys.stderr)
        return 1
    except Exception as exc:  # Keep unexpected diagnostics redacted as well.
        report["error"] = _redact(f"unexpected browser E2E failure: {exc}", (args.admin_password, args.expected_node_name))
        _write_report(args.report, report, (args.admin_password, args.expected_node_name))
        print(f"browser E2E failed: {report['error']}", file=sys.stderr)
        return 1
    _write_report(args.report, report, (args.admin_password, args.expected_node_name))
    print("browser E2E passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
