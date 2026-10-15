# Jev bounded live acceptance

This opt-in harness calls the **production adapters** using public or synthetic
inputs. It never starts application workers, writes user memory, changes routing,
or migrates databases. Running it can incur provider charges. Keys are read from
the environment, never CLI arguments or output files.

1. Prepare a **new output directory** from the existing public frozen dataset:

   ```sh
   python3 scripts/jev-acceptance/prepare.py .tmp/jev-acceptance-NEW \
     --frozen-public-input /path/to/public/combined.json
   go build -o .tmp/jev-acceptance-NEW/jev-acceptance ./scripts/jev-acceptance
   ```

2. Securely supply `BKCRAB_JEV_ENDPOINT`, `BKCRAB_JEV_API_KEY` and a pinned
   `BKCRAB_JEV_MODEL`. This run used the prior experiment's OpenRouter channel:
   endpoint `https://openrouter.ai/api/alpha/decisions`, model
   `typesafe/jev-1.13-20260917`. Do not use an OpenRouter key with a TypeSafe URL.

3. Run one input at a time, keeping a shared **overall** budget across commands:

   ```sh
   .tmp/jev-acceptance-NEW/jev-acceptance \
     -input .tmp/jev-acceptance-NEW/guards.json -budget-usd 0.08 \
     > .tmp/jev-acceptance-NEW/guards.jsonl
   ```

   Each process has its own cumulative estimate, with $0.01 reserved before each
   Decide including retries. This is not a provider billing guarantee. The CLI
   caps its budget at $0.50; use smaller portions when running multiple commands.
   Rows, including failures, are emitted as JSONL; inspect statuses rather than
   treating exit 0 as model success. Never overwrite previous results to retry.

4. `-qwen-endpoint http://127.0.0.1:18081/v1` selects a local Qwen baseline for
   rerank rows. Confirm the actual bound port before use. Qwen mode still validates
   Jev config but does not call it; a non-secret dummy key suffices for this arm.
   Do not send internal Docker IP traffic through an external HTTP proxy.

5. `-proxy` starts an authenticated ephemeral loopback bridge. Supply a random
   `JEV_ACCEPTANCE_INTERNAL_KEY` of at least 24 characters. It prints its URL once,
   then usage-only call records. The bridge accepts only the `acceptance` owner and
   the fixed production `jeveval` rubric; it exits after ten minutes. This is **not**
   a test of deployed Gateway authorization or routing. Terminate after the smoke.

6. `compare_ragas.py OUTPUT.jsonl` uses current `services/rag-evaluator/app` code.
   Supply its usual LLM/embedding settings and the local bridge URL/model/internal
   key through `RAG_EVALUATOR_*`. The two selected samples don't call embedding,
   but the engine still requires a configured embedding entry. Use
   `RAG_EVALUATOR_LLM_ENDPOINT=https://api.deepseek.com/v1`,
   `RAG_EVALUATOR_LLM_MODEL=deepseek-flash`, output cap 2048, input/output prices
   0.3/1.2. The experiment-only transport disables thinking and SDK retries,
   preserves tool_choice, and reserves $0.02 per attempted LLM call under an
   overall $0.20 estimate. Do not reuse it for other pricing without updating it.

   ```sh
   .tmp/jev-eval-venv/bin/python scripts/jev-acceptance/compare_ragas.py \
     .tmp/jev-acceptance-NEW/ragas.jsonl
   ```

7. `analyze.py ROOT OUTPUT.json` summarizes the frozen first-round shape (33 guard
   cases, 8 judge requests, 4 Jev replays, 2 Qwen replays and 16 metric results).
   It requires the named files used by the recorded experiment; it is not a
   general benchmark CLI. Preserve the `smoke`, `guards`, `judges`, `rerank`,
   initial `qwen`, corrected `qwen-local`, and `ragas` JSONL files. Failed metrics
   stay unavailable. Ragas costs include both the LLM generation and Jev verdicts.

The fixture labels are author expectations, not independently annotated truth.
English old replay labels identify documents, not passage-level relevance;
Chinese candidates are constructed. Do not tune on these examples and then
reuse them as held-out proof. Read the
observed results and limitations in local `docs/jev-acceptance-results-2026-10-07.md` (experiment records are not included in this source commit).

Offline checks:

```sh
go test ./scripts/jev-acceptance
.tmp/jev-eval-venv/bin/ruff check scripts/jev-acceptance --ignore ASYNC230
```

The sole ASYNC230 exception permits flushing a tiny local result row between
serial network calls so interrupted paid runs retain their results.
