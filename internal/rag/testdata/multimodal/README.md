# Multimodal RAG test corpus

This directory contains a small, repository-owned corpus for the deterministic
parts of multimodal document RAG. All prose, diagrams, tables, code, and attack
strings were written for this repository and are dedicated to the public domain
under **CC0-1.0**. No production document, credential, customer data, or
third-party copyrighted sample is included.

The corpus deliberately separates two things:

- `canonical-artifact.json` is a parser-output fixture with two page locations,
  one stable image asset/occurrence, a GFM table, a fenced Go block, caption,
  OCR text, and exact expected chunk boundaries in `expected.json`.
- `adversarial.md` and the `*-injection.json` files contain inert attack text
  covering raw HTML, unsafe links/images, forged internal schemes, prompt/tool/
  metadata instructions, and oversized/deep typed DocumentAI responses.

Regenerate all checked-in files from the repository root:

```bash
go run ./internal/rag/testdata/multimodal/generate.go \
  -output ./internal/rag/testdata/multimodal
```

Review the diff after regeneration. Tests consume the checked-in expected
values; they do not regenerate expectations at runtime, so a parser or splitter
contract change remains visible as a golden diff.

The Python sidecar has a complementary source-document generator at
`services/rag-parser/tests/fixtures/generate_multimodal.py`. It creates a real
four-page PDF with embedded images plus malicious OOXML containers in a test
temporary directory.
