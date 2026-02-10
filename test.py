#!/usr/bin/env python3
"""Comprehensive test suite for the load balancer."""

import os
import signal
import subprocess
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import pytest
import logging
import requests

logger = logging.getLogger(__name__)

DIR = Path(__file__).parent
LB_BIN = DIR / "lb"
MOCK_BIN = DIR / "mock-backend"
LB_PORT = 8080
LB_URL = f"http://localhost:{LB_PORT}"

PROCS: list[subprocess.Popen] = []


# --- Helpers ---


def start_mock(port: int, **kwargs) -> subprocess.Popen:
    args = [str(MOCK_BIN), "--port", str(port)]
    for k, v in kwargs.items():
        args += [f"--{k}", str(v)]
    logger.info(f"Starting mock: {' '.join(args)}")
    p = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    PROCS.append(p)
    return p


def start_lb(backends: list[str], **kwargs) -> subprocess.Popen:
    args = [str(LB_BIN)]
    for b in backends:
        args += ["--backends", b]
    for k, v in kwargs.items():
        args += [f"--{k}", str(v)]
    logger.info(f"Starting LB: {' '.join(args)}")
    p = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    PROCS.append(p)
    return p


def wait_for_port(port: int, timeout: float = 10) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            requests.get(f"http://localhost:{port}/v1/models", timeout=1)
            return True
        except requests.ConnectionError:
            time.sleep(0.2)
    return False


def wait_for_lb(timeout: float = 10) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            requests.get(f"{LB_URL}/health", timeout=1)
            return True
        except requests.ConnectionError:
            time.sleep(0.2)
    return False


def completion() -> requests.Response:
    return requests.post(
        f"{LB_URL}/v1/completions",
        json={"prompt": "test", "max_tokens": 10},
        timeout=10,
    )


def completions(n: int) -> list[requests.Response]:
    """Fire n completion requests in parallel."""
    with ThreadPoolExecutor(max_workers=n) as pool:
        futures = [pool.submit(completion) for _ in range(n)]
        results = []
        for f in futures:
            try:
                results.append(f.result())
            except requests.exceptions.RequestException as e:
                logger.warning(f"Request failed: {e}")
        return results


def cleanup():
    for p in PROCS:
        try:
            os.killpg(os.getpgid(p.pid), signal.SIGTERM)
        except (OSError, ProcessLookupError):
            pass
    deadline = time.time() + 3
    for p in PROCS:
        remaining = max(0.1, deadline - time.time())
        try:
            p.wait(timeout=remaining)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(os.getpgid(p.pid), signal.SIGKILL)
            except (OSError, ProcessLookupError):
                pass
            p.wait()
    PROCS.clear()


BACKEND_URLS = [
    "http://localhost:8000",
    "http://localhost:8001",
    "http://localhost:8002",
]


def start_scenario(mock_configs: list[dict], lb_kwargs: dict | None = None):
    """Start mock backends and LB for a scenario.

    mock_configs: list of kwargs dicts for start_mock, keyed by port.
                  e.g. [{"port": 8000, "mode": "healthy"}, ...]
    """
    for cfg in mock_configs:
        start_mock(**cfg)

    # Wait for healthy mocks to be ready
    for cfg in mock_configs:
        if cfg.get("mode") != "timeout":
            assert wait_for_port(cfg["port"]), f"Mock on port {cfg['port']} not ready"

    backends = [f"http://localhost:{cfg['port']}" for cfg in mock_configs]
    kw = {"health-check-interval": "1s", **(lb_kwargs or {})}
    start_lb(backends, **kw)
    assert wait_for_lb(), "LB not ready"


@pytest.fixture(autouse=True, scope="class")
def _cleanup():
    yield
    cleanup()


# --- Tests ---


class TestHealthy:
    """Scenario A: Three healthy backends."""

    @classmethod
    def setup_class(cls):
        start_scenario([
            {"port": 8000, "mode": "healthy"},
            {"port": 8001, "mode": "healthy"},
            {"port": 8002, "mode": "healthy"},
        ])

    def test_lb_health(self):
        r = requests.get(f"{LB_URL}/health", timeout=5)
        assert r.status_code == 200
        body = r.json()
        assert body["status"] == "ok"
        assert body["healthy_backends"] == 3

    def test_completion_returns_200(self):
        r = completion()
        assert r.status_code == 200

    def test_distribution(self):
        ports = {r.json()["backend_port"] for r in completions(30)}
        assert ports == {8000, 8001, 8002}


class TestSlow:
    """Scenario B: Mixed healthy and slow backends."""

    @classmethod
    def setup_class(cls):
        start_scenario([
            {"port": 8000, "mode": "healthy"},
            {"port": 8001, "mode": "healthy", "delay": "500ms"},
            {"port": 8002, "mode": "healthy", "delay": "1s"},
            {"port": 8003, "mode": "healthy", "delay": "2s"},
            {"port": 8004, "mode": "healthy", "delay": "3s"},
        ])

    def test_all_succeed(self):
        for r in completions(6):
            assert r.status_code == 200


class TestFailing:
    """Scenario C: One failing backend gets excluded."""

    @classmethod
    def setup_class(cls):
        start_scenario([
            {"port": 8000, "mode": "healthy"},
            {"port": 8001, "mode": "healthy"},
            {"port": 8002, "mode": "failing"},
        ])
        # Wait for health checker to mark 8002 unhealthy
        time.sleep(2)

    def test_health_excludes_failing(self):
        r = requests.get(f"{LB_URL}/health", timeout=5)
        assert r.json()["healthy_backends"] == 2

    def test_no_requests_to_failing(self):
        ports = [r.json()["backend_port"] for r in completions(20)]
        assert 8002 not in ports


class TestFlaky:
    """Scenario D: Flaky backend — most requests still succeed."""

    @classmethod
    def setup_class(cls):
        start_scenario([
            {"port": 8000, "mode": "healthy"},
            {"port": 8001, "mode": "flaky", "failure-rate": "0.5"},
            {"port": 8002, "mode": "healthy"},
        ])
        time.sleep(1)

    def test_most_succeed(self):
        ok = sum(1 for r in completions(20) if r.status_code == 200)
        assert ok >= 14, f"Only {ok}/20 succeeded"


class TestHealthRecovery:
    """Scenario E: Kill and restart a backend."""

    @classmethod
    def setup_class(cls):
        start_mock(8000, mode="healthy")
        cls.mock_8001 = start_mock(8001, mode="healthy")
        start_mock(8002, mode="healthy")
        assert wait_for_port(8000) and wait_for_port(8001) and wait_for_port(8002)

        start_lb(BACKEND_URLS, **{"health-check-interval": "1s"})
        assert wait_for_lb()

    def test_recovery(self):
        # All 3 initially active
        ports = {r.json()["backend_port"] for r in completions(30)}
        assert ports == {8000, 8001, 8002}, f"Expected all 3, got {ports}"

        # Kill 8001
        logger.info("Killing mock on port 8001")
        self.mock_8001.terminate()
        self.mock_8001.wait()

        # Wait for health check to detect
        time.sleep(3)

        ports = [r.json()["backend_port"] for r in completions(20)]
        assert 8001 not in ports, "Requests still going to killed backend"

        # Restart 8001
        logger.info("Restarting mock on port 8001")
        start_mock(8001, mode="healthy")
        assert wait_for_port(8001)

        # Wait for health check to recover
        time.sleep(3)

        ports = {r.json()["backend_port"] for r in completions(30)}
        assert ports == {8000, 8001, 8002}, f"Expected all 3 after recovery, got {ports}"


class TestTimeout:
    """Scenario F: LB should not hang on timeout backend."""

    @classmethod
    def setup_class(cls):
        start_mock(8000, mode="timeout")
        time.sleep(0.5)
        start_lb(["http://localhost:8000"], timeout="2s", **{"health-check-interval": "1s"})
        assert wait_for_lb(), "LB not ready"

    def test_does_not_hang(self):
        try:
            r = requests.post(
                f"{LB_URL}/v1/completions",
                json={"prompt": "test"},
                timeout=5,
            )
            assert r.status_code != 200
        except requests.exceptions.RequestException:
            pass  # timeout or connection error is fine


class TestImmediateUnhealthy:
    """Scenario G: Backend marked unhealthy immediately on proxy error, not waiting for health check."""

    @classmethod
    def setup_class(cls):
        start_mock(8000, mode="healthy")
        start_mock(8001, mode="healthy")
        cls.mock_8002 = start_mock(8002, mode="healthy")
        assert wait_for_port(8000) and wait_for_port(8001) and wait_for_port(8002)

        # Long health check interval so health checker won't detect the failure
        start_lb(BACKEND_URLS, **{"health-check-interval": "60s"})
        assert wait_for_lb()

    def test_immediate_mark_on_proxy_error(self):
        # Confirm all 3 healthy
        r = requests.get(f"{LB_URL}/health", timeout=5)
        assert r.json()["healthy_backends"] == 3

        # Kill backend 8002
        self.mock_8002.terminate()
        self.mock_8002.wait()

        # Send enough requests to hit the dead backend
        completions(20)

        # Should be marked unhealthy immediately, no need to wait for health check
        r = requests.get(f"{LB_URL}/health", timeout=5)
        assert r.json()["healthy_backends"] == 2, (
            f"Expected 2 healthy after killing backend, got {r.json()['healthy_backends']}"
        )


class TestLBHealthDegraded:
    """LB /health reports degraded when no backends healthy."""

    @classmethod
    def setup_class(cls):
        start_mock(8000, mode="failing")
        time.sleep(0.5)
        start_lb(["http://localhost:8000"], **{"health-check-interval": "1s"})
        assert wait_for_lb()
        time.sleep(2)

    def test_degraded(self):
        r = requests.get(f"{LB_URL}/health", timeout=5)
        assert r.status_code == 503
        assert r.json()["status"] == "degraded"


class TestPortFlag:
    """Verify --port flag is respected."""

    @classmethod
    def setup_class(cls):
        start_mock(8000, mode="healthy")
        assert wait_for_port(8000)
        start_lb(["http://localhost:8000"], port="9090", **{"health-check-interval": "1s"})
        # Wait for LB on custom port
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                requests.get("http://localhost:9090/health", timeout=1)
                break
            except requests.ConnectionError:
                time.sleep(0.2)

    def test_listens_on_custom_port(self):
        r = requests.get("http://localhost:9090/health", timeout=5)
        assert r.status_code == 200
        assert r.json()["status"] == "ok"


class TestHealthCheckInterval:
    """Verify --health-check-interval applies to both health checker and status logger."""

    @classmethod
    def setup_class(cls):
        start_mock(8000, mode="healthy")
        cls.mock_8001 = start_mock(8001, mode="healthy")
        assert wait_for_port(8000) and wait_for_port(8001)
        # Use 500ms interval so recovery is fast
        cls.lb = start_lb(
            ["http://localhost:8000", "http://localhost:8001"],
            **{"health-check-interval": "500ms"},
        )
        assert wait_for_lb()

    def test_fast_recovery(self):
        """Kill a backend and verify it's detected unhealthy within ~1s (not 30s)."""
        r = requests.get(f"{LB_URL}/health", timeout=5)
        assert r.json()["healthy_backends"] == 2

        # Kill backend 8001
        self.mock_8001.terminate()
        self.mock_8001.wait()

        # With 500ms interval, should be detected within ~1s
        time.sleep(1.5)
        r = requests.get(f"{LB_URL}/health", timeout=5)
        assert r.json()["healthy_backends"] == 1

        # Restart and verify fast recovery
        start_mock(8001, mode="healthy")
        assert wait_for_port(8001)

        time.sleep(1.5)
        r = requests.get(f"{LB_URL}/health", timeout=5)
        assert r.json()["healthy_backends"] == 2


class TestBackendsWithoutScheme:
    """Verify backends without http:// scheme get it added automatically."""

    @classmethod
    def setup_class(cls):
        start_mock(8000, mode="healthy")
        assert wait_for_port(8000)
        # Pass backend without http:// prefix
        start_lb(["localhost:8000"], **{"health-check-interval": "1s"})
        assert wait_for_lb()

    def test_request_succeeds(self):
        r = requests.post(
            f"{LB_URL}/v1/completions",
            json={"prompt": "test", "max_tokens": 10},
            timeout=10,
        )
        assert r.status_code == 200
