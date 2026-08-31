import test from "node:test";
import assert from "node:assert/strict";

const {
  formatParserEvalDuration,
  formatParserEvalScore,
  nextParserEvalPollDelay,
  parserEvalCanRetry,
  parserEvalExtension,
  parserEvalMimeType,
  parserEvalRunIsActive,
  patchParserEvalUpload,
  validateParserEvalFiles,
} = await import(new URL("./parser-eval-state.ts", import.meta.url));

const limits = { maxFiles: 2, maxFileBytes: 100, maxBatchBytes: 150 };

test("parser evaluation accepts only bounded OOXML office files", () => {
  const result = validateParserEvalFiles(
    [{ name: "existing.docx", size: 40, lastModified: 1 }],
    [
      { name: "slides.PPTX", size: 50, lastModified: 2 },
      { name: "notes.pdf", size: 10, lastModified: 3 },
      { name: "huge.xlsx", size: 101, lastModified: 4 },
      { name: "third.docx", size: 10, lastModified: 5 },
    ],
    limits,
  );
  assert.deepEqual(result.accepted.map((file) => file.name), ["slides.PPTX"]);
  assert.equal(result.errors.length, 3);
  assert.equal(parserEvalExtension("book.XLSX"), "xlsx");
  assert.equal(parserEvalExtension("archive.xlsx.zip"), "");
  assert.match(parserEvalMimeType("slides.pptx"), /presentationml/);
});

test("parser evaluation rejects duplicates and total byte overflow", () => {
  const duplicate = { name: "same.docx", size: 60, lastModified: 7 };
  const result = validateParserEvalFiles([duplicate], [duplicate, { name: "book.xlsx", size: 100 }], limits);
  assert.deepEqual(result.accepted, []);
  assert.match(result.errors.join(" "), /已在上传队列/);
  assert.match(result.errors.join(" "), /批次总大小/);
});

test("upload transitions update one file without losing its stable key", () => {
  const current = [{ id: "one", file: { name: "a.docx", size: 12 }, idempotencyKey: "stable", status: "pending", progress: 0 }];
  const next = patchParserEvalUpload(current, "one", { status: "failed", progress: 42, error: "timeout" });
  assert.equal(next[0].idempotencyKey, "stable");
  assert.equal(next[0].status, "failed");
  assert.equal(next[0].progress, 42);
});

test("active and terminal actions drive polling and retry", () => {
  assert.equal(parserEvalRunIsActive("QUEUED"), true);
  assert.equal(parserEvalRunIsActive("PARTIAL"), false);
  assert.equal(parserEvalCanRetry("PARTIAL"), true);
  assert.equal(parserEvalCanRetry("CANCELLED"), false);
  assert.equal(nextParserEvalPollDelay(true, false), 4_000);
  assert.equal(nextParserEvalPollDelay(true, true), 30_000);
  assert.equal(nextParserEvalPollDelay(false, false), null);
});

test("missing measurements render as em dash rather than zero", () => {
  assert.equal(formatParserEvalDuration(undefined), "—");
  assert.equal(formatParserEvalScore(undefined), "—");
  assert.equal(formatParserEvalDuration(250), "250 ms");
  assert.equal(formatParserEvalScore(87.25), "87.3");
});
