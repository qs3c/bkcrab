#!/usr/bin/env python3
"""Summarize a read-only, projected RAG evaluation JSONL export.

Input kinds: run, result, metric, case. This script makes no network calls,
does not run models, and never infers disabled-stage quality from old traces.
"""

import argparse
import json
import math
import statistics
from collections import Counter
from pathlib import Path


def decode(value, default):
    if value is None or value == "":
        return default
    return json.loads(value) if isinstance(value, str) else value


def aggregate(values):
    values = sorted(v for v in values if v is not None and math.isfinite(v))
    if not values:
        return {"n": 0, "mean": None, "p50": None, "p95": None}

    def percentile(p):
        position = (len(values) - 1) * p
        lo, hi = math.floor(position), math.ceil(position)
        return values[lo] + (values[hi] - values[lo]) * (position - lo)

    return {"n": len(values), "mean": statistics.mean(values),
            "p50": percentile(.5), "p95": percentile(.95)}


def summarize(rows):
    output = []
    for run in (r for r in rows if r["kind"] == "run"):
        results = [r for r in rows if r["kind"] == "result" and r["runId"] == run["id"]]
        metrics = [r for r in rows if r["kind"] == "metric" and r["runId"] == run["id"]]
        cases = [r for r in rows if r["kind"] == "case"
                 and r["datasetVersionId"] == run["datasetVersionId"]]
        saved = [decode(r.get("searchTrace"), {}) for r in results]
        traces = [r.get("trace", r) for r in saved]
        hits = [h for s in saved for h in s.get("hits", [])]
        ranked = [h for h in hits if h.get("rerankScore") is not None]
        removed = [h for h in ranked if h.get("selected") is False]
        stage_ms = {}
        for name in ("plannerDurationMs", "rerankerDurationMs", "retrievalDurationMs",
                     "hydrationDurationMs", "durationMs"):
            stage_ms[name] = aggregate([t[name] for t in traces if name in t])
        stage_ms["caseLatencyMs"] = aggregate([r.get("latencyMs") for r in results])
        metric_summary = {}
        for name in sorted({r["metric"] for r in metrics}):
            items = [r for r in metrics if r["metric"] == name]
            valid = [r["value"] for r in items if r["status"] == "ok"
                     and r["value"] is not None and math.isfinite(r["value"])]
            metric_summary[name] = {"count": len(items), "statuses": dict(Counter(r["status"] for r in items)),
                                    **aggregate(valid)}
        snapshot = decode(run.get("snapshot"), {})
        source = decode(run.get("source"), {})
        output.append({
            "runId": run["id"], "datasetDisplayName": run["datasetName"], "source": source,
            "datasetVersionId": run["datasetVersionId"], "generationId": run.get("generationId"),
            "generationDatasetVersionId": run.get("generationDatasetVersionId"),
            "profile": snapshot.get("profile"), "caseCount": run["caseCount"],
            "documentCount": run["documentCount"], "status": run["status"],
            "caseStatuses": dict(Counter(r["status"] for r in results)), "stagesMs": stage_ms,
            "traceCoverage": len(traces), "plannerFallbackCases": sum(t.get("plannerFallback") is True for t in traces),
            "rerankerSucceededCases": sum(t.get("rerankerSucceeded") is True for t in traces),
            "emptyContextCases": sum(t.get("returnedCount") == 0 for t in traces),
            "caseLabels": {"history": sum(bool(decode(c.get("history"), [])) for c in cases),
                           "expectedAbstention": sum(bool(c.get("expectedAbstention")) for c in cases),
                           "chunkGold": sum(bool(decode(c.get("referenceContextIds"), [])) for c in cases)},
            "savedTopNFilter": {"ranked": len(ranked), "removed": len(removed),
                                "affectedCases": sum(any(h.get("selected") is False for h in s.get("hits", [])) for s in saved),
                                "removedFromGoldDocuments": sum(h.get("relevant") is True for h in removed),
                                "removedFromOtherDocuments": sum(h.get("relevant") is False for h in removed),
                                "removedWithoutLabels": sum(h.get("relevant") is None for h in removed)},
            "metrics": metric_summary,
        })
    return {"note": "Historical descriptive statistics only; missing or failed scores are not zero. "
                    "Saved top-N reranker hits cannot reconstruct the original candidate top-K. "
                    "Document-level labels do not establish chunk-level relevance.", "runs": output}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    rows = [json.loads(line) for line in args.input.read_text().splitlines() if line.strip()]
    report = summarize(rows)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(f"Summarized {len(report['runs'])} historical runs into {args.output}")


if __name__ == "__main__":
    main()
