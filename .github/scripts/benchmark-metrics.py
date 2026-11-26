#!/usr/bin/env python3
"""Convert Go samples to BMF and verify the load report before and after upload."""

import argparse
import json
import math
from pathlib import Path


GO_METRICS = {
    "ns/op": ("latency", "nanoseconds (ns)"),
    "B/op": ("alloc-bytes-per-op", "bytes / operation"),
    "allocs/op": ("allocs-per-op", "allocations / operation"),
    "wire-B/op": ("wire-bytes-per-op", "bytes / operation"),
    "body-B/op": ("body-bytes-per-op", "bytes / operation"),
    "wire-B/msg": ("wire-bytes-per-message", "bytes / message"),
    "body-B/msg": ("body-bytes-per-message", "bytes / message"),
    "compressed-B/op": ("compressed-bytes-per-op", "bytes / operation"),
    "p50-us/op": ("p50-latency", "microseconds (µs)"),
    "p99-us/op": ("p99-latency", "microseconds (µs)"),
    "ttfb-us/op": ("time-to-running", "microseconds (µs)"),
    "peak-heap-B": ("peak-heap", "bytes (B)"),
    "peak-disk-B": ("peak-disk", "bytes (B)"),
    "overlap-%": ("worker-overlap", "percent (%)"),
    "start-us/op": ("worker-start-latency", "microseconds (µs)"),
}

DISK_MEASURES = {
    "disk-inuse", "peak-disk", "sqlite-db-disk", "peak-sqlite-wal-disk",
    "data-files-disk", "peak-data-files-disk",
}

LOAD_METRICS = {
    **dict.fromkeys(sorted(DISK_MEASURES), "bytes (B)"),
    "alloc-bytes-per-req": "bytes / request",
    "heap-inuse": "bytes (B)",
    "peak-heap": "bytes (B)",
    "peak-rss": "bytes (B)",
    "cpu-time": "microseconds / request",
    "gc-pause": "microseconds / request",
    "wire-bytes-per-read": "bytes / read request",
    "body-bytes-per-read": "bytes / read request",
    "cache-hit-rate": "percent (%)",
    "write-throughput": "requests / second",
    "write-latency": "nanoseconds (ns)",
    "sessions-list-latency": "nanoseconds (ns)",
    "transcript-latency": "nanoseconds (ns)",
}

LOAD_LATENCIES = {"latency", "write-latency", "sessions-list-latency", "transcript-latency"}


def check(condition, message):
    if not condition:
        raise ValueError(message)


def go_report(path):
    benchmarks = {}
    for line in path.read_text().splitlines():
        if not line.startswith("Benchmark"):
            continue
        name, iterations, *columns = line.split()
        check(int(iterations) > 0 and len(columns) % 2 == 0, f"malformed Go result: {line}")
        check(name not in benchmarks, f"multiple results for {name}")
        measures = {}
        for value, unit in zip(columns[::2], columns[1::2]):
            check(unit in GO_METRICS, f"unmapped Go metric {unit} for {name}")
            measure, _ = GO_METRICS[unit]
            sample = float(value)
            check(math.isfinite(sample) and sample >= 0, f"invalid Go metric {unit} for {name}")
            measures[measure] = {"value": sample}
        required = {"latency", "alloc-bytes-per-op", "allocs-per-op"}
        check(required <= measures.keys(), f"missing standard Go metrics for {name}")
        benchmarks[name] = measures
    check(benchmarks, "no Go benchmark results")
    return benchmarks


def load_variants():
    levels = (1, 4, 16, 64)
    return {
        "CoreAPI/SessionsList": [
            {"concurrency": c, "sessions": n} for n in (256, 2000) for c in levels
        ],
        "CoreAPI/Transcript": [
            {"concurrency": c, "page": page, "turns": 200}
            for page in ("latest", "full")
            for c in levels
        ],
        "CoreAPI/Mixed": [
            {"concurrency": c, "sessions": 262, "turns": 120, "writers": 2} for c in levels[1:]
        ],
        "CoreAPI/TranscriptFullStream": [
            {"concurrency": c, "turns": 600} for c in (1, 64)
        ],
    }


def load_key(benchmark, parameters):
    return (benchmark, json.dumps(parameters, sort_keys=True))


def load_report(path, linux):
    report = json.loads(path.read_text())
    variants = load_variants()
    check(report.keys() == variants.keys(), "unexpected load benchmark names")
    keys = {}
    for benchmark, parameters in variants.items():
        required = {"latency", "throughput", "alloc-bytes-per-req", "peak-heap", "heap-inuse",
                    "gc-pause", "wire-bytes-per-read"} | DISK_MEASURES
        if linux:
            required |= {"cpu-time", "peak-rss"}
        if benchmark != "CoreAPI/SessionsList":
            required.add("cache-hit-rate")
        if benchmark != "CoreAPI/TranscriptFullStream":
            required.add("body-bytes-per-read")
        if benchmark == "CoreAPI/Mixed":
            required |= {"write-latency", "write-throughput", "sessions-list-latency", "transcript-latency"}
        observed = []
        for variant in report[benchmark]:
            params, measures = variant["parameters"], variant["measures"]
            observed.append(params)
            key = load_key(benchmark, params)
            check(key not in keys, f"duplicate load variant {key}")
            check(required <= measures.keys(), f"missing load metrics for {key}: {required - measures.keys()}")
            check(measures.keys() <= LOAD_METRICS.keys() | {"latency", "throughput"},
                  f"unknown load metrics for {key}")
            if "cache-hit-rate" in measures:
                hit_rate = measures["cache-hit-rate"]["value"]
                check(0 <= hit_rate <= 100, f"invalid cache hit rate for {key}")
            if benchmark == "CoreAPI/TranscriptFullStream":
                check(measures["wire-bytes-per-read"]["value"] > 512 * 1024 and hit_rate == 0,
                      f"streaming fixture was cacheable for {key}")
            for measure, metrics in measures.items():
                names = {"value", "p50", "p99"} if measure in LOAD_LATENCIES else {"value"}
                check(metrics.keys() == names, f"invalid metric keys for {key} / {measure}")
                check(all(math.isfinite(v) and v >= 0 for v in metrics.values()),
                      f"invalid numeric metric for {key} / {measure}")
                if measure in LOAD_LATENCIES:
                    check(0 < metrics["p50"] <= metrics["p99"], f"invalid percentiles for {key} / {measure}")
            disk = {name: measures[name]["value"] for name in DISK_MEASURES}
            check(0 < disk["sqlite-db-disk"]
                  and disk["sqlite-db-disk"] + disk["data-files-disk"] <= disk["disk-inuse"] <= disk["peak-disk"]
                  and disk["peak-sqlite-wal-disk"] <= disk["peak-disk"]
                  and disk["data-files-disk"] <= disk["peak-data-files-disk"] <= disk["peak-disk"],
                  f"inconsistent disk usage for {key}")
            keys[key] = measures
        check(len(observed) == len(parameters) and all(p in observed for p in parameters),
              f"missing or unexpected parameters for {benchmark}")
    return keys


def check_published(path, expected):
    report = json.loads(path.read_text())
    if "bmf_version" in report or "parse_outcome" in report:
        parsed = report.get("bmf_version") == 1 and report.get("parse_outcome") == "parsed"
    else:
        # Older self-hosted Bencher releases identify the format by adapter.
        parsed = report.get("adapter") == "json_v1"
    check(parsed and report.get("uuid"), "Bencher failed to parse the BMF v1 report")
    batches = report["results"]
    check(len(batches) == 1 and len(batches[0]) == len(expected), "Bencher changed the variant count")
    observed = set()
    for variant in batches[0]:
        key = load_key(variant["benchmark"]["name"], variant["variant"]["parameters"])
        check(key in expected and key not in observed, f"unexpected Bencher variant {key}")
        observed.add(key)
        received = {m["measure"]["slug"]: {v["name"]: v["value"] for v in m["metrics"]}
                    for m in variant["measures"]}
        check(len(received) == len(variant["measures"]) and received.keys() == expected[key].keys(),
              f"Bencher changed the measures for {key}")
        for measure, metrics in expected[key].items():
            check(received[measure].keys() == metrics.keys()
                  and all(math.isclose(received[measure][name], value) for name, value in metrics.items()),
                  f"Bencher changed {measure} metrics for {key}")
    return report["uuid"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    convert = commands.add_parser("go")
    convert.add_argument("path", type=Path)
    validate = commands.add_parser("check-load")
    validate.add_argument("path", type=Path)
    validate.add_argument("--linux", action="store_true")
    validate.add_argument("--published", type=Path)
    commands.add_parser("measures")
    args = parser.parse_args()
    if args.command == "go":
        print(json.dumps(go_report(args.path), indent=2, allow_nan=False))
    elif args.command == "check-load":
        variants = load_report(args.path, args.linux)
        if args.published:
            print(check_published(args.published, variants))
        else:
            print(f"Verified {len(variants)} load variants")
    else:
        units = dict(GO_METRICS.values()) | LOAD_METRICS | {"raw-bytes": "bytes (B)", "gzip-bytes": "bytes (B)"}
        del units["latency"]  # Bencher provides latency and throughput by default.
        print(json.dumps(units, indent=2))


if __name__ == "__main__":
    main()
