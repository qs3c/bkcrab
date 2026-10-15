"""Summarize diagnostic results without converting failures to quality zeros."""

import argparse
import hashlib
import json
import statistics
from collections import Counter
from pathlib import Path

p = argparse.ArgumentParser()
p.add_argument("root", type=Path)
p.add_argument("output", type=Path)
a = p.parse_args()


def rows(name):
    return [
        json.loads(line) for line in (a.root / f"{name}.jsonl").read_text().splitlines()
    ]


def timing(values):
    return {
        "n": len(values),
        "meanMs": statistics.mean(values) if values else None,
        "minMs": min(values) if values else None,
        "maxMs": max(values) if values else None,
    }


g = rows("guards")
assert len(g) == 33
known = [r for r in g if r["status"] == "ok"]
guard = {
    "cases": len(g),
    "successful": len(known),
    "languages": sorted({r["language"] for r in g}),
    "actions": dict(Counter(r.get("action") for r in g)),
    "benign": sum(r["expected"] == "allow" for r in g),
    "risky": sum(r["expected"] == "reject" for r in g),
    "falseAllows": [
        r["id"] for r in known if r["expected"] == "reject" and r["action"] == "allow"
    ],
    "falseRejects": [
        {"id": r["id"], "action": r["action"], "calls": r["calls"]}
        for r in known
        if r["expected"] == "allow" and r["action"] != "allow"
    ],
    "remoteCalls": sum(len(r["calls"] or []) for r in g),
    "timing": timing([r["durationMs"] for r in known if r["calls"]]),
}
judges = rows("judges")
verdicts = []
for r in judges:
    if r["status"] != "ok":
        continue
    for i, expected in enumerate(r["expected"].split(",")):
        probability = r["verdict"]["result"]["probabilities"][f"c{i}"]
        verdicts.append(
            {
                "id": r["id"],
                "index": i,
                "expected": int(expected),
                "actual": int(probability >= 0.5),
                "probability": probability,
            }
        )
paired = []
reranks = {r["id"]: r for r in rows("rerank")}
for q in rows("qwen-local"):
    j = reranks[q["id"]]
    paired.append(
        {
            "id": q["id"],
            "language": q["language"],
            "jevStatus": j["status"],
            "qwenStatus": q["status"],
            "jevMs": j["durationMs"],
            "qwenMs": q["durationMs"],
            "jevTop5": [v["Index"] for v in j.get("ranks", [])[:5]],
            "qwenTop5": [v["Index"] for v in q.get("ranks", [])[:5]],
            "knownRelevant": q["relevant"],
        }
    )
ragas = rows("ragas")
assert len(ragas) == 16
bundles = {}
for bundle in sorted({r["bundle"] for r in ragas}):
    selected = [r for r in ragas if r["bundle"] == bundle]
    bundles[bundle] = {
        "metrics": len(selected),
        "ok": sum(r["result"]["status"] == "ok" for r in selected),
        "durationMs": sum(r["durationMs"] for r in selected),
        "llmRequests": sum(len(r["llmCalls"]) for r in selected),
        "llmEstimatedCostUsd": sum(
            sum(c["estimatedCostUsd"] for c in r["llmCalls"]) for r in selected
        ),
        "jevEstimatedCostUsd": sum(
            r["usage"].get("jevEstimatedCostUsd", 0) for r in selected
        ),
    }
comparisons = []
for case in sorted({r["caseId"] for r in ragas}):
    for metric in sorted({r["metric"] for r in ragas}):
        selected = [r for r in ragas if r["caseId"] == case and r["metric"] == metric]
        comparisons.append(
            {
                "caseId": case,
                "metric": metric,
                "values": {r["bundle"]: r["result"].get("value") for r in selected},
                "statuses": {r["bundle"]: r["result"]["status"] for r in selected},
            }
        )
baseCost = sum(
    rows(n)[-1]["cumulativeEstimatedCostUsd"]
    for n in ["smoke", "guards", "judges", "rerank"]
)
summary = {
    "date": "2026-10-07",
    "scope": "bounded real-provider diagnostic; not production rollout or held-out acceptance",
    "model": "typesafe/jev-1.13-20260917",
    "guard": guard,
    "judge": {
        "requests": len(judges),
        "verdicts": len(verdicts),
        "mismatches": [v for v in verdicts if v["expected"] != v["actual"]],
    },
    "rerank": {
        "jevCases": len(reranks),
        "jevSucceeded": sum(r["status"] == "ok" for r in reranks.values()),
        "pairs": paired,
        "initialQwenTransportFailures": len(rows("qwen")),
    },
    "ragas": {"bundles": bundles, "comparisons": comparisons},
    "totalEstimatedCostUsd": baseCost
    + sum(
        b["llmEstimatedCostUsd"] + b["jevEstimatedCostUsd"] for b in bundles.values()
    ),
    "hashes": {
        f.name: hashlib.sha256(f.read_bytes()).hexdigest()
        for f in sorted(a.root.glob("*.json*"))
        if f.name != a.output.name
    },
    "limits": [
        "synthetic labels authored with samples, not independent ground truth",
        "only two bilingual Ragas samples",
        "historical English document labels do not establish passage relevance",
        "Chinese retrieval candidates are constructed diagnostics",
        "no repeated-run stability, load, live traffic, or final-answer-quality gate",
    ],
}
a.output.write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n")
print(
    json.dumps(
        {
            "estimatedCostUsd": summary["totalEstimatedCostUsd"],
            "guardFalseRejects": len(guard["falseRejects"]),
            "judgeMismatches": len(summary["judge"]["mismatches"]),
            "ragas": bundles,
        },
        ensure_ascii=False,
    )
)
