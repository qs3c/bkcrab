# Jev reranker experiment

This is an isolated experiment, not a production routing switch. The Go runner
reuses bkcrab's evaluation retrieval, Qwen3 client, answer prompt, and answer
generator. It does not migrate the database or start application workers.

## Reproduction

1. Build `cmd/rag-rerank-bench` for Linux amd64 with `CGO_ENABLED=0`; record its
   SHA-256 and source commit. Put the binary in a private experiment directory
   on the deployment host. Push source changes before pulling on that host.
2. Use `docker_run.py DIRECTORY CONTAINER_NAME ...` with names beginning
   `jev-bench-`. It reuses the live image's CA bundle and storage/model bindings,
   runs as the invoking user, and exposes no ports. It does not copy secrets to
   files. For Jev, stdin accepts `{"openrouterKey":"..."}` supplied securely by
   the caller; do not put real credentials in history, source, or logs.
3. Run `-mode freeze -output english.json` to retrieve fresh Top20 from the
   existing public evaluation generation. Override owner/source-run/generation
   flags if reproducing against different public data. Keep private data out of
   this experiment unless separately authorized for external processing.
4. Download CMRC2018 dev at the revision recorded in `prepare_chinese.py` and
   run that script. Its injected/removed-gold diagnostics are not real RRF
   retrieval; report them separately. The gold fields never enter model input.
5. Pilot Jev with `-split calibration -arms jev -warmups 0 -repeats 1` using
   `-jev-batch 1 -jev-concurrency 4` and `-jev-batch 5 -jev-concurrency 2` in
   separate outputs. Freeze a configuration before inspecting formal outcomes.
6. Run `-mode rank -input combined.json -output formal-ranks.jsonl -repeats 3
   -warmups 5`. Cases are serial, arm order uses seed 20260921, both rerankers
   score the entire candidate set, and there is no score filter or fallback.
7. After a successful answer/judge smoke, run `-mode answer -input combined.json
   -ranks formal-ranks.jsonl -output formal-answers.jsonl`. This uses only
   successful repeat-zero ranks. Output hashes bind the exact input ranks file;
   finish or snapshot that file before running answers. Failed outcomes are
   not automatically retried. Use a different named output for an explicitly
   documented recovery attempt.
8. `evaluate.py INPUT ANSWERS OUTPUT` sends saved public answers through the
   existing Docker evaluator. Token budget and duration are bounded. Dollar
   prices in the existing deployment can be placeholders: report token usage
   and do not present those estimates as actual charges.
9. `analyze.py INPUT RANKS SUMMARY --answers ANSWERS --scores SCORES` produces
   statistics. Supply a language-specific input view to keep strata separate;
   it ignores results outside that view. It excludes failed metrics rather than
   treating them as zero, and computes paired, source-clustered bootstrap CIs.

`status.py DIRECTORY` reads compact progress without reading credentials.
Capture container logs and image IDs before removing only the experiment's
own stopped containers. Preserve append-only raw records and manifests.

## Interpretation

- Warmups are excluded from formal latency. Report success-only latency and
  failure counts/latencies together; a fast failed request is not a speed gain.
- Repeated queries are repeated timings, not additional independent questions.
- English document recall cannot establish chunk relevance. Chinese gold labels
  cover constructed evidence, not every potentially relevant distractor.
- Answer generation timing and frozen retrieval/re-ranking timing are separate
  stages. Their sum is a replay estimate, never a measured online end-to-end
  latency. Judge time is excluded from the user-facing latency.
- The API is experimental. Retain actual returned Jev model IDs and split
  results if the provider changes versions.

## Checks

```text
go test -timeout 60s ./internal/rag/rerank ./cmd/rag-rerank-bench
python -m unittest discover -s scripts/jev-reranker -p 'test_*.py'
```
