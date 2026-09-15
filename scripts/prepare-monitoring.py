#!/usr/bin/env python3
"""Generate a reviewable config without editing or restarting shared monitoring."""
import argparse
import copy
import hashlib
import json
import os
import tempfile
from pathlib import Path
import yaml


def integrate(base, relay):
    result = copy.deepcopy(base)
    jobs = result.setdefault("scrape_configs", [])
    if any(job.get("job_name") == "relay-metrics" for job in jobs):
        raise ValueError("relay-metrics already exists; review the existing integration")
    jobs.extend(relay["scrape_configs"])
    rules = result.setdefault("rule_files", [])
    if "/etc/prometheus/relay/alerts.yml" not in rules:
        rules.append("/etc/prometheus/relay/alerts.yml")
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("source", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    if args.source.resolve() == args.output.resolve():
        raise ValueError("source and output must differ")
    raw = args.source.read_bytes()
    base = yaml.safe_load(raw)
    fragment = Path(__file__).resolve().parents[1] / "deploy/monitoring/scrape.yml"
    result = integrate(base, yaml.safe_load(fragment.read_text()))
    args.output.parent.mkdir(parents=True, exist_ok=True)
    # JSON is valid YAML; preserve settings rather than rewriting the shared file.
    with tempfile.NamedTemporaryFile(mode="w", dir=args.output.parent, delete=False) as temp:
        temp.write(json.dumps(result, indent=2) + "\n")
        temporary = Path(temp.name)
    temporary.chmod(args.source.stat().st_mode & 0o777)
    os.replace(temporary, args.output)
    args.output.with_suffix(".source.sha256").write_text(hashlib.sha256(raw).hexdigest() + "\n")
    print("Generated Relay integration; validate and compare before deployment.")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, TypeError, AttributeError, yaml.YAMLError):
        raise SystemExit("Cannot prepare monitoring config; check source syntax, existing Relay jobs and output permissions.") from None
