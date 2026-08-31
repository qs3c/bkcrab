"use client";

import { useRef, useState, type DragEvent } from "react";
import { useRouter } from "next/navigation";
import { FilePlus2, Play, RotateCcw, Trash2, UploadCloud } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import {
  createParserEvalRun,
  parserEvalIdempotencyKey,
  removeParserEvalDocument,
  startParserEvalRun,
  uploadParserEvalDocument,
  type ParserEvalCapabilities,
} from "@/lib/api";
import {
  formatParserEvalBytes,
  parserEvalMimeType,
  patchParserEvalUpload,
  validateParserEvalFiles,
  type ParserEvalUploadState,
} from "../parser-eval-state";

type UploadItem = ParserEvalUploadState<File>;

export function CreateRunPanel({ capabilities, onCreated }: { capabilities: ParserEvalCapabilities; onCreated: () => Promise<void> }) {
  const router = useRouter();
  const inputRef = useRef<HTMLInputElement>(null);
  const createKey = useRef(parserEvalIdempotencyKey());
  const startKey = useRef(parserEvalIdempotencyKey());
  const [judgeBindingId, setJudgeBindingId] = useState(capabilities.judgeModelBindings[0]?.id ?? "");
  const [uploads, setUploads] = useState<UploadItem[]>([]);
  const [runId, setRunId] = useState("");
  const [busy, setBusy] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [error, setError] = useState("");

  function addFiles(files: File[]) {
    const validation = validateParserEvalFiles(uploads.map((item) => item.file), files, capabilities);
    const normalized = validation.accepted.map((raw) => {
      const file = raw as File;
      const normalizedFile = file.type === parserEvalMimeType(file.name)
        ? file
        : new File([file], file.name, { type: parserEvalMimeType(file.name), lastModified: file.lastModified });
      return {
        id: parserEvalIdempotencyKey(),
        file: normalizedFile,
        idempotencyKey: parserEvalIdempotencyKey(),
        status: "pending" as const,
        progress: 0,
      };
    });
    setUploads((current) => [...current, ...normalized]);
    setError(validation.errors.join("；"));
  }

  function drop(event: DragEvent<HTMLDivElement>) {
    event.preventDefault();
    setDragging(false);
    addFiles(Array.from(event.dataTransfer.files));
  }

  async function remove(item: UploadItem) {
    if (busy) return;
    setError("");
    try {
      if (runId && item.documentId) await removeParserEvalDocument(runId, item.documentId);
      setUploads((current) => current.filter((candidate) => candidate.id !== item.id));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "移除文档失败");
    }
  }

  async function submit() {
    if (!judgeBindingId || uploads.length === 0 || !capabilities.available) return;
    setBusy(true);
    setError("");
    try {
      let targetRunID = runId;
      if (!targetRunID) {
        const run = await createParserEvalRun(judgeBindingId, createKey.current);
        targetRunID = run.id;
        setRunId(run.id);
      }
      let failed = false;
      for (const item of uploads.filter((candidate) => candidate.status !== "uploaded")) {
        setUploads((current) => patchParserEvalUpload(current, item.id, { status: "uploading", progress: 0, error: undefined }));
        try {
          const document = await uploadParserEvalDocument(
            targetRunID,
            item.file,
            item.idempotencyKey,
            (progress) => setUploads((current) => patchParserEvalUpload(current, item.id, { progress })),
          );
          setUploads((current) => patchParserEvalUpload(current, item.id, {
            status: "uploaded", progress: 100, documentId: document.id, error: undefined,
          }));
        } catch (cause) {
          failed = true;
          setUploads((current) => patchParserEvalUpload(current, item.id, {
            status: "failed", error: cause instanceof Error ? cause.message : "上传失败",
          }));
        }
      }
      if (failed) {
        setError("部分文档上传失败。修正后点击“重试并启动”，已成功的文件不会重复上传。");
        return;
      }
      await startParserEvalRun(targetRunID, startKey.current);
      await onCreated();
      router.push(`/admin/parser-evals/${encodeURIComponent(targetRunID)}/`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "创建解析测评失败");
    } finally {
      setBusy(false);
    }
  }

  const uploadedCount = uploads.filter((item) => item.status === "uploaded").length;
  const totalBytes = uploads.reduce((sum, item) => sum + item.file.size, 0);

  return <Card>
    <CardHeader>
      <CardTitle className="flex items-center gap-2"><FilePlus2 className="h-5 w-5" />新建解析测评</CardTitle>
      <CardDescription>上传 Office 文档后，仅执行页面真值生成、MarkItDown / AnyDoc 双解析和视觉裁判评分，不进入分块、向量化或检索流程。</CardDescription>
    </CardHeader>
    <CardContent className="space-y-5">
      <div className="space-y-2">
        <Label htmlFor="parser-eval-judge">本次任务裁判模型</Label>
        <select
          id="parser-eval-judge"
          className="h-10 w-full rounded-md border bg-background px-3 text-sm"
          value={judgeBindingId}
          disabled={busy || !!runId}
          onChange={(event) => setJudgeBindingId(event.target.value)}
        >
          <option value="">选择支持图片输入的模型</option>
          {capabilities.judgeModelBindings.map((binding) => <option key={binding.id} value={binding.id}>
            {binding.modelDisplayName || binding.model} · {binding.provider}
          </option>)}
        </select>
        <p className="text-xs text-muted-foreground">任务启动时会冻结模型、解析器、renderer 和评分 Prompt 版本；后续配置变化不会静默替换裁判。</p>
      </div>

      <div
        className={`rounded-lg border-2 border-dashed p-7 text-center transition-colors ${dragging ? "border-primary bg-primary/5" : "border-border"}`}
        onDragEnter={(event) => { event.preventDefault(); setDragging(true); }}
        onDragOver={(event) => event.preventDefault()}
        onDragLeave={() => setDragging(false)}
        onDrop={drop}
      >
        <UploadCloud className="mx-auto h-8 w-8 text-muted-foreground" />
        <p className="mt-2 text-sm font-medium">拖入 .docx / .pptx / .xlsx</p>
        <p className="mt-1 text-xs text-muted-foreground">最多 {capabilities.maxFiles} 个 · 单文件 {formatParserEvalBytes(capabilities.maxFileBytes)} · 批次 {formatParserEvalBytes(capabilities.maxBatchBytes)}</p>
        <input ref={inputRef} className="hidden" type="file" multiple accept=".docx,.pptx,.xlsx" onChange={(event) => { addFiles(Array.from(event.target.files ?? [])); event.target.value = ""; }} />
        <Button className="mt-4" type="button" variant="outline" disabled={busy} onClick={() => inputRef.current?.click()}>选择文件</Button>
      </div>

      {uploads.length > 0 && <div className="space-y-2">
        <div className="flex items-center justify-between text-xs text-muted-foreground"><span>{uploads.length} 个文件 · {formatParserEvalBytes(totalBytes)}</span><span>{uploadedCount}/{uploads.length} 已上传</span></div>
        {uploads.map((item) => <div key={item.id} className="rounded-md border p-3">
          <div className="flex items-center gap-3">
            <div className="min-w-0 flex-1"><p className="truncate text-sm font-medium">{item.file.name}</p><p className="text-xs text-muted-foreground">{formatParserEvalBytes(item.file.size)} · {{ pending: "待上传", uploading: `上传中 ${item.progress}%`, uploaded: "已上传", failed: "上传失败" }[item.status]}</p></div>
            {item.status === "failed" && <RotateCcw className="h-4 w-4 text-amber-500" />}
            <Button type="button" size="icon-sm" variant="ghost" disabled={busy} aria-label={`移除 ${item.file.name}`} onClick={() => void remove(item)}><Trash2 className="h-4 w-4" /></Button>
          </div>
          {item.status === "uploading" && <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary transition-[width]" style={{ width: `${item.progress}%` }} /></div>}
          {item.error && <p className="mt-2 text-xs text-destructive">{item.error}</p>}
        </div>)}
      </div>}

      {error && <p className="rounded-md bg-destructive/10 p-3 text-sm text-destructive">{error}</p>}
      <div className="flex items-center justify-between gap-3">
        <p className="text-xs text-muted-foreground">每份文档按固定顺序执行两次解析；延迟同时记录纯解析耗时与端到端耗时。</p>
        <Button disabled={busy || !capabilities.available || !judgeBindingId || uploads.length === 0} onClick={() => void submit()}>
          {busy ? <UploadCloud className="mr-2 h-4 w-4 animate-pulse" /> : <Play className="mr-2 h-4 w-4" />}
          {busy ? "处理中" : runId ? "重试并启动" : "上传并启动"}
        </Button>
      </div>
    </CardContent>
  </Card>;
}
