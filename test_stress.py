#!/usr/bin/env python3
"""Stress test: 20 backends, 1000 concurrent requests via asyncio + aiohttp."""

import asyncio
import os
import signal
import subprocess
import time
from collections import Counter
from pathlib import Path

import aiohttp
import requests

DIR = Path(__file__).parent
LB_BIN = DIR / "lb"
MOCK_BIN = DIR / "mock-backend"
LB_PORT = 8080
LB_URL = f"http://localhost:{LB_PORT}"

NUM_BACKENDS = 20
BASE_PORT = 8000
NUM_REQUESTS = 1000

PROCS: list[subprocess.Popen] = []


def start_proc(args: list[str]) -> subprocess.Popen:
    p = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    PROCS.append(p)
    return p


def cleanup():
    for p in PROCS:
        try:
            os.killpg(os.getpgid(p.pid), signal.SIGTERM)
        except (OSError, ProcessLookupError):
            pass
    deadline = time.time() + 5
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


def wait_for_port(port: int, timeout: float = 10) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            requests.get(f"http://localhost:{port}/v1/models", timeout=1)
            return True
        except requests.ConnectionError:
            time.sleep(0.2)
    return False


def wait_for_lb(timeout: float = 15) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            requests.get(f"{LB_URL}/health", timeout=1)
            return True
        except requests.ConnectionError:
            time.sleep(0.2)
    return False


async def run_stress():
    ports = list(range(BASE_PORT, BASE_PORT + NUM_BACKENDS))

    # Start mock backends
    print(f"Starting {NUM_BACKENDS} mock backends on ports {ports[0]}-{ports[-1]}")
    for port in ports:
        start_proc([str(MOCK_BIN), "--port", str(port), "--mode", "healthy", "--delay", "2s"])

    for port in ports:
        assert wait_for_port(port), f"Mock on port {port} not ready"
    print("All mock backends ready")

    # Start LB
    args = [str(LB_BIN)]
    for port in ports:
        args += ["--backends", f"http://localhost:{port}"]
    args += ["--health-check-interval", "1s"]
    start_proc(args)
    assert wait_for_lb(), "LB not ready"
    print("LB ready")

    # Fire requests
    print(f"Sending {NUM_REQUESTS} concurrent requests...")
    payload = {"prompt": "test", "max_tokens": 10}
    ok = 0
    failed = 0
    backend_ports: list[int] = []
    t0 = time.time()

    connector = aiohttp.TCPConnector(limit=0)
    async with aiohttp.ClientSession(connector=connector) as session:
        async def do_request():
            try:
                async with session.post(
                    f"{LB_URL}/v1/completions",
                    json=payload,
                    timeout=aiohttp.ClientTimeout(total=30),
                ) as resp:
                    if resp.status == 200:
                        body = await resp.json()
                        return ("ok", body.get("backend_port"))
                    return ("fail", resp.status)
            except Exception as e:
                return ("error", str(e))

        results = await asyncio.gather(*[do_request() for _ in range(NUM_REQUESTS)])

    elapsed = time.time() - t0

    for status, val in results:
        if status == "ok":
            ok += 1
            backend_ports.append(val)
        else:
            failed += 1

    # Report
    dist = Counter(backend_ports)
    rps = NUM_REQUESTS / elapsed

    print(f"Completed in {elapsed:.2f}s ({rps:.0f} req/s)")
    print(f"Success: {ok}/{NUM_REQUESTS}, Failed: {failed}/{NUM_REQUESTS}")
    print(f"Backends hit: {len(dist)}/{NUM_BACKENDS}")

    if dist:
        counts = sorted(dist.values())
        print(f"Distribution: min={counts[0]}, max={counts[-1]}, median={counts[len(counts)//2]}")
        for port in sorted(dist):
            print(f"  :{port} -> {dist[port]} requests")

    # Assertions
    assert ok > NUM_REQUESTS * 0.95, f"Too many failures: {failed}/{NUM_REQUESTS}"
    assert len(dist) == NUM_BACKENDS, f"Only {len(dist)}/{NUM_BACKENDS} backends received traffic"
    print("Stress test passed")


def main():
    try:
        asyncio.run(run_stress())
    finally:
        cleanup()


if __name__ == "__main__":
    main()
