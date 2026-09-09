#!/usr/bin/env python3
"""Offline, paired analysis of the four September 2026 ablations and A supplement.

Rejects duplicate or incomplete records. Failed metric results stay missing;
successful scores are never selected by best-of-retries. Confidence intervals
resample reference-document groups to keep questions from the same paper together.
"""
import argparse
from collections import Counter, defaultdict
import hashlib
import json
import math
from pathlib import Path
import random
import statistics

from summarize import aggregate, decode, summarize

RUNS = {
    "A": "rer_74edcb129e6a9a252f5c4fdc6c8b0969",
    "B": "rer_992d50417cb34dab71da4024e0ef4007",
    "C": "rer_b2eb9facbdb6e3cbfb1b79fcd66f9b94",
    "D": "rer_8db2c5d1cd82d94d79f58e8f6a29cbb1",
}
LLM = {"context_precision", "context_recall", "faithfulness", "response_relevancy", "factual_correctness"}


def percentile(values, p):
    values = sorted(values)
    pos = (len(values)-1)*p
    lo, hi = math.floor(pos), math.ceil(pos)
    return values[lo] + (values[hi]-values[lo])*(pos-lo)


def paired(base, candidate, clusters, seed, repetitions=10000):
    keys = sorted(base.keys() & candidate.keys())
    if not keys:
        return {"n": 0}
    differences = [candidate[k]-base[k] for k in keys]
    groups = defaultdict(list)
    for key, difference in zip(keys, differences):
        groups[clusters[key]].append(difference)
    blocks = [(sum(values), len(values)) for values in groups.values()]
    rng = random.Random(seed)
    draws = []
    for _ in range(repetitions):
        chosen = rng.choices(blocks, k=len(blocks))
        draws.append(sum(b[0] for b in chosen)/sum(b[1] for b in chosen))
    return {
        "n": len(keys), "referenceDocumentGroups": len(blocks),
        "baseMean": statistics.mean(base[k] for k in keys),
        "candidateMean": statistics.mean(candidate[k] for k in keys),
        "delta": statistics.mean(differences),
        "ci95": [percentile(draws, .025), percentile(draws, .975)],
        "higher": sum(d > 1e-9 for d in differences),
        "lower": sum(d < -1e-9 for d in differences),
        "equal": sum(abs(d) <= 1e-9 for d in differences),
        "caseIds": keys,
    }


def analyze(export_path, supplement_path):
    exported = [json.loads(line) for line in export_path.read_text().splitlines()]
    supplements = [json.loads(line) for line in supplement_path.read_text().splitlines()]
    rows = [r for r in exported if r["kind"] == "case" or r.get("id", r.get("runId")) in RUNS.values()]
    runs = {r["id"]: r for r in rows if r["kind"] == "run"}
    assert set(runs) == set(RUNS.values())
    assert len({r["generationId"] for r in runs.values()}) == 1
    assert len({r["datasetVersionId"] for r in runs.values()}) == 1
    profiles = {name: decode(runs[run]["snapshot"], {})["profile"] for name, run in RUNS.items()}
    stable = []
    for name, profile in profiles.items():
        expected = {"A": (True, True, True), "B": (False, False, True),
                    "C": (True, True, False), "D": (False, False, False)}[name]
        assert tuple(profile[k] for k in ("rewriteEnabled", "hydeEnabled", "rerankerEnabled")) == expected
        stable.append({k: v for k, v in profile.items() if k not in ("rewriteEnabled", "hydeEnabled", "rerankerEnabled")})
    assert all(p == stable[0] for p in stable)

    seen = {(r["runId"], r["caseId"], r["metric"]) for r in rows if r["kind"] == "metric"}
    assert len(seen) == sum(r["kind"] == "metric" for r in rows)
    assert len(supplements) == 48
    for supplement in supplements:
        assert supplement["sourceRunId"] == RUNS["A"] and supplement["judgeMaxTokens"] == 8192
        response = supplement["response"]
        assert response["ragasVersion"] == "0.3.9" and response["metricBundleVersion"] == "rag-core-v1"
        assert len(response["results"]) == 1
        item = response["results"][0]
        assert set(item["metrics"]) == LLM
        for metric, result in item["metrics"].items():
            key = (RUNS["A"], item["caseId"], metric)
            assert key not in seen, f"duplicate scoring record: {key}"
            seen.add(key)
            rows.append({"kind": "metric", "runId": RUNS["A"], "caseId": item["caseId"],
                         "metric": metric, "version": "rag-core-v1", "source": "supplement", **result})

    cases = {r["caseId"]: r for r in rows if r["kind"] == "case"}
    assert len(cases) == 50
    clusters = {key: tuple(sorted(decode(case["referenceDocumentIds"], []))) or (key,) for key, case in cases.items()}
    results = {}
    scores = {}
    for name, run in RUNS.items():
        saved = [r for r in rows if r["kind"] == "result" and r["runId"] == run]
        assert len(saved) == len({r["caseId"] for r in saved}) == 50
        assert {r["caseId"] for r in saved} == set(cases)
        results[name] = {r["caseId"]: r for r in saved}
        ok_cases = {r["caseId"] for r in saved if r["status"] == "ok"}
        expected_metrics = decode(runs[run]["snapshot"], {})["metrics"]
        for case in ok_cases:
            assert all((run, case, metric) in seen for metric in expected_metrics)
        items = [r for r in rows if r["kind"] == "metric" and r["runId"] == run]
        scores[name] = {}
        for metric in expected_metrics:
            scores[name][metric] = {r["caseId"]: r["value"] for r in items
                                    if r["metric"] == metric and r["status"] == "ok"}
        for key in ("plannerDurationMs", "rerankerDurationMs", "durationMs"):
            scores[name][key] = {r["caseId"]: decode(r["searchTrace"], {})["trace"][key] for r in saved}
        scores[name]["successfulCaseLatencyMs"] = {r["caseId"]: r["latencyMs"] for r in saved if r["status"] == "ok"}

    aggregated = {r["runId"]: r for r in summarize(rows)["runs"]}
    summaries = {name: aggregated[run] for name, run in RUNS.items()}
    for name, summary in summaries.items():
        summary["successfulCaseLatencyMs"] = aggregate(list(scores[name]["successfulCaseLatencyMs"].values()))
        errors = [r for r in rows if r["kind"] == "metric" and r["runId"] == RUNS[name] and r["status"] == "error"]
        summary["metricErrors"] = dict(Counter("timeout" if "timeout" in r["reason"].lower()
                                              else "missing_tool_calls" if "tool_calls" in r["reason"]
                                              else "other" for r in errors))
    comparisons = {}
    for base, candidate in [("A", "B"), ("A", "C"), ("A", "D"), ("B", "D"), ("C", "D")]:
        name = candidate + "-" + base
        comparisons[name] = {}
        for metric in scores[base]:
            seed = int(hashlib.sha256((name+metric).encode()).hexdigest()[:16], 16)
            comparisons[name][metric] = paired(scores[base][metric], scores[candidate][metric], clusters, seed)
    return {
        "method": "candidate minus base; valid within-case intersection; 10000 paired reference-document-group bootstrap draws; pointwise percentile 95% intervals, no multiple-comparison correction; errors are missing, never zero; not an equivalence test",
        "inputSHA256": {"export": hashlib.sha256(export_path.read_bytes()).hexdigest(), "supplement": hashlib.sha256(supplement_path.read_bytes()).hexdigest()},
        "supplementCompletedAt": supplements[-1]["completedAt"],
        "supplementCases": len(supplements), "supplementSeconds": sum(r["durationSeconds"] for r in supplements),
        "supplementTokens": supplements[-1]["cumulativeSupplementTokens"],
        "referenceDocumentGroups": len(set(clusters.values())),
        "summaries": summaries, "comparisons": comparisons,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("export", type=Path)
    parser.add_argument("supplement", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    report = analyze(args.export, args.supplement)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2)+"\n")
    for name, summary in report["summaries"].items():
        print(name, {metric: (round(summary["metrics"][metric]["mean"], 4), summary["metrics"][metric]["n"])
                     for metric in sorted(LLM)})
    for name, metrics in report["comparisons"].items():
        print(name, {metric: {k: v for k, v in metrics[metric].items() if k in ("n", "delta", "ci95")}
                     for metric in sorted(LLM)})


if __name__ == "__main__":
    main()
