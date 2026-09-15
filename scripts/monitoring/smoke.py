#!/usr/bin/env python3
"""Validate real Prometheus scrapes and provisioned Grafana dashboards in isolation.

Requires Go, Docker Compose, Python 3. No model/storage credentials or Python deps.
--keep retains the temporary stack for browser inspection; cleanup.json records
the exact cleanup command. Default execution always removes its own containers.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--keep", action="store_true")
    args = parser.parse_args()
    work = Path(tempfile.mkdtemp(prefix="bkcrab-monitoring-smoke-"))
    password = secrets.token_urlsafe(24)
    env = dict(os.environ, GRAFANA_ADMIN_USER="smoke", GRAFANA_ADMIN_PASSWORD=password,
               GRAFANA_PORT="0", PROMETHEUS_PORT="0", CGO_ENABLED="0")
    base = work / "compose.json"
    base.write_text(json.dumps({"services": {"bkcrab": {
        "image": "prom/prometheus:v3.14.0", "entrypoint": ["/fixture"],
        "volumes": [str(work / "fixture") + ":/fixture:ro"]}}}))
    command = ["docker", "compose", "--project-name", work.name, "--project-directory",
               str(ROOT / "deploy/docker"), "-f", str(base), "-f",
               str(ROOT / "deploy/docker/docker-compose.monitoring.yml")]

    def run(*arguments, capture=False):
        return subprocess.run(arguments, cwd=ROOT, env=env, check=True, text=True,
                              stdout=subprocess.PIPE if capture else None).stdout

    def get(url, auth=False):
        request = urllib.request.Request(url)
        if auth:
            token = base64.b64encode(("smoke:" + password).encode()).decode()
            request.add_header("Authorization", "Basic " + token)
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.load(response)

    cleanup = ["env", "GRAFANA_ADMIN_PASSWORD=cleanup-only", "GRAFANA_PORT=0", "PROMETHEUS_PORT=0"] + command + ["down", "-v"]
    (work / "cleanup.json").write_text(json.dumps(cleanup))
    success = False
    try:
        run("go", "build", "-o", str(work / "fixture"), "./scripts/monitoring/smoke")
        run(*command, "up", "-d")
        prom = "http://" + run(*command, "port", "prometheus", "9090", capture=True).strip()
        grafana = "http://" + run(*command, "port", "grafana", "3000", capture=True).strip()
        auth_file = work / "browser.json"
        auth_file.write_text(json.dumps({"url": grafana, "username": "smoke", "password": password}))
        auth_file.chmod(0o600)
        deadline = time.monotonic() + 100
        while True:
            try:
                targets = get(prom + "/api/v1/targets")["data"]["activeTargets"]
                target = next(t for t in targets if t["labels"].get("job") == "bkcrab")
                assert target["health"] == "up", target.get("lastError")
                assert get(grafana + "/api/health")["database"] == "ok"
                source = get(grafana + "/api/datasources/uid/bkcrab-prometheus/health", True)
                assert source["status"] == "OK", source
                break
            except Exception:
                if time.monotonic() > deadline:
                    raise
                time.sleep(2)
        # At least two scrapes for rate queries; poll values instead of a fixed sleep.
        query = 'sum(rate(bkcrab_http_requests_total{job="bkcrab"}[1m]))'
        while True:
            data = get(prom + "/api/v1/query?" + urllib.parse.urlencode({"query": query}))
            if data["data"]["result"]:
                break
            if time.monotonic() > deadline:
                raise RuntimeError("rate series missing after multiple scrapes")
            time.sleep(2)
        checked = 0
        for path in sorted((ROOT / "deploy/monitoring/grafana/dashboards").glob("*.json")):
            dashboard = json.loads(path.read_text())
            loaded = get(grafana + "/api/dashboards/uid/" + dashboard["uid"], True)
            assert loaded["meta"]["provisioned"], dashboard["uid"]
            assert len(loaded["dashboard"]["panels"]) == len(dashboard["panels"])
            for panel in dashboard["panels"]:
                expression = panel["targets"][0]["expr"].replace("$__rate_interval", "1m").replace("$__range", "1h").replace("$instance", ".*")
                query_path = "/api/v1/query?" + urllib.parse.urlencode({"query": expression})
                # Through Grafana's actual datasource proxy, not just Prometheus.
                result = get(grafana + "/api/datasources/proxy/uid/bkcrab-prometheus" + query_path, True)
                assert result["status"] == "success", (panel["title"], result)
                assert result["data"]["result"], (panel["title"], "no fixture series")
                checked += 1
        rules = get(prom + "/api/v1/rules")["data"]["groups"]
        assert sum(len(group["rules"]) for group in rules) == 7
        success = True
        print(json.dumps({"scrape": "up", "dashboards": 3, "panel_queries": checked,
                          "rules": 7, "grafana": grafana, "workdir": str(work)}, ensure_ascii=False))
    finally:
        if args.keep and success:
            print("Temporary stack retained; browser credentials and cleanup command are in " + str(work))
        else:
            result = subprocess.run(command + ["down", "-v"], cwd=ROOT, env=env, check=False)
            if result.returncode == 0:
                shutil.rmtree(work)
            else:
                raise RuntimeError("Cleanup failed; retry the command in " + str(work / "cleanup.json"))


if __name__ == "__main__":
    main()
