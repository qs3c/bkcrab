"use client";

import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { DatabaseZap, Download, Loader2, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  cancelRAGEvalCatalogImport,
  createRAGEvalCatalogImport,
  getRAGEvalCatalog,
  listRAGEvalCatalogImports,
  type RAGEvalCatalogImport,
  type RAGEvalCatalogPreset,
} from "@/lib/api";

function importProgress(job: RAGEvalCatalogImport): string {
  try {
    const value = JSON.parse(job.progressJson) as { documents?: number; cases?: number };
    if (value.documents || value.cases) return `${value.documents ?? 0} 文档 · ${value.cases ?? 0} 样例`;
  } catch { /* stored payload is best-effort display data */ }
  return job.stage;
}

function catalogCorpusSize(preset: RAGEvalCatalogPreset, split: string): number {
  return preset.corpusSizes?.[split] || preset.defaultCorpusSize || 1;
}

function defaultCorpusLimit(preset: RAGEvalCatalogPreset, track: "TEXT_RAG" | "PDF_E2E", split: string): number {
  const corpusSize = catalogCorpusSize(preset, split);
  if (track === "PDF_E2E") return Math.min(50, corpusSize);
  return corpusSize;
}

export function CatalogImportPanel({ maxRunCases, onChanged, onDatasetSelected }: {
  maxRunCases: number;
  onChanged: () => Promise<void>;
  onDatasetSelected?: (id: string) => void;
}) {
  const [catalog, setCatalog] = useState<RAGEvalCatalogPreset[]>([]);
  const [imports, setImports] = useState<RAGEvalCatalogImport[]>([]);
  const [catalogId, setCatalogId] = useState("");
  const [track, setTrack] = useState<"TEXT_RAG" | "PDF_E2E">("TEXT_RAG");
  const [split, setSplit] = useState("");
  const [sampleSize, setSampleSize] = useState(0);
  const [seed, setSeed] = useState(42);
  const [corpusLimit, setCorpusLimit] = useState(50);
  const [evidenceTypes, setEvidenceTypes] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const previouslyActive = useRef(false);
  const preset = useMemo(() => catalog.find((item) => item.id === catalogId), [catalog, catalogId]);
  const active = imports.some((item) => item.status === "QUEUED" || item.status === "RUNNING");
  const recentImports = useMemo(() => [...imports]
    .sort((left, right) => String(right.createdAt).localeCompare(String(left.createdAt)))
    .slice(0, 5), [imports]);

  const refreshImports = useCallback(async () => {
    try {
      const next = await listRAGEvalCatalogImports();
      const nextActive = next.some((item) => item.status === "QUEUED" || item.status === "RUNNING");
      setImports(next);
      if (previouslyActive.current && !nextActive) await onChanged();
      previouslyActive.current = nextActive;
    } catch (err) { setError(err instanceof Error ? err.message : "读取准备任务失败"); }
  }, [onChanged]);

  useEffect(() => {
    const timer = window.setTimeout(() => void Promise.all([
      getRAGEvalCatalog().then((items) => {
        setCatalog(items);
        if (items.length > 0) setCatalogId((current) => current || items[0].id);
      }),
      refreshImports(),
    ]), 0);
    return () => window.clearTimeout(timer);
  }, [refreshImports]);

  useEffect(() => {
    if (!active) return;
    const timer = window.setTimeout(() => void refreshImports(), 3000);
    return () => window.clearTimeout(timer);
  }, [active, imports, refreshImports]);

  useEffect(() => {
    if (!preset) return;
    const defaultTrack = preset.tracks[0];
    const defaultSplit = preset.splits[0];
    setTrack(defaultTrack);
    setSplit(defaultSplit);
    setSampleSize(Math.min(preset.defaultSampleSize, maxRunCases || preset.defaultSampleSize));
    setEvidenceTypes(preset.evidenceTypes?.includes("text") ? ["text"] : []);
    setCorpusLimit(defaultCorpusLimit(preset, defaultTrack, defaultSplit));
  }, [preset, maxRunCases]);

  async function startImport(event: FormEvent) {
    event.preventDefault();
    if (!preset) return;
    setBusy(true); setError("");
    try {
      const created = await createRAGEvalCatalogImport({
        catalogId: preset.id, track, split, sampleSize, seed,
        evidenceTypes: preset.id === "vectara-open-ragbench" ? evidenceTypes : undefined,
        corpusLimit,
      });
      await onChanged();
      onDatasetSelected?.(created.datasetId);
      await refreshImports();
    } catch (err) { setError(err instanceof Error ? err.message : "创建准备任务失败"); }
    finally { setBusy(false); }
  }

  async function cancel(id: string) {
    try { await cancelRAGEvalCatalogImport(id); await refreshImports(); }
    catch (err) { setError(err instanceof Error ? err.message : "取消失败"); }
  }

  return <Card>
    <CardHeader>
      <CardTitle className="flex items-center gap-2"><DatabaseZap className="h-5 w-5" />内置测评数据集</CardTitle>
      <CardDescription>只选择数据集和规模。服务器负责下载、缓存、转换、校验并冻结版本；同一上游文件只下载一次。</CardDescription>
    </CardHeader>
    <CardContent className="space-y-5">
      <form className="space-y-4" onSubmit={startImport}>
        <div className="space-y-2"><Label>数据集</Label><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={catalogId} onChange={(event) => setCatalogId(event.target.value)}>{catalog.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></div>
        {preset && <div className="rounded-md bg-muted p-3 text-xs text-muted-foreground"><p>{preset.description}</p><p className="mt-1 font-mono">commit {preset.revision.slice(0, 12)} · {preset.license}</p></div>}
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-2"><Label>测评轨道</Label><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={track} onChange={(event) => { const next = event.target.value as "TEXT_RAG" | "PDF_E2E"; setTrack(next); if (preset) setCorpusLimit(defaultCorpusLimit(preset, next, split)); }}>{preset?.tracks.map((item) => <option key={item} value={item}>{item === "TEXT_RAG" ? "文本 RAG" : "PDF 端到端"}</option>)}</select></div>
          <div className="space-y-2"><Label>Split</Label><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={split} onChange={(event) => { const next = event.target.value; setSplit(next); if (preset) setCorpusLimit(defaultCorpusLimit(preset, track, next)); }}>{preset?.splits.map((item) => <option key={item} value={item}>{item}</option>)}</select></div>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-2"><Label htmlFor="catalog-samples">样例数</Label><Input id="catalog-samples" type="number" min={1} max={Math.min(preset?.maxSampleSize || 1, maxRunCases || Number.MAX_SAFE_INTEGER)} value={sampleSize} onChange={(event) => setSampleSize(Number(event.target.value))} /><p className="text-[11px] text-muted-foreground">当前单次运行上限 {maxRunCases || "—"}。</p></div>
          <div className="space-y-2"><Label htmlFor="catalog-seed">随机种子</Label><Input id="catalog-seed" type="number" value={seed} onChange={(event) => setSeed(Number(event.target.value))} /><p className="text-[11px] text-muted-foreground">相同种子会稳定抽到同一批样例。</p></div>
        </div>
        {preset?.evidenceTypes && <div className="space-y-2"><Label>证据类型</Label><div className="flex flex-wrap gap-3">{preset.evidenceTypes.map((item) => <label key={item} className="flex items-center gap-1.5 text-xs"><input type="checkbox" checked={evidenceTypes.includes(item)} onChange={(event) => setEvidenceTypes((current) => event.target.checked ? [...current, item] : current.filter((value) => value !== item))} />{item}</label>)}</div></div>}
        {preset && <div className="space-y-2"><Label htmlFor="catalog-corpus-limit">{track === "PDF_E2E" ? "PDF 语料数" : "文本语料数"}</Label><Input id="catalog-corpus-limit" type="number" min={1} max={catalogCorpusSize(preset, split)} value={corpusLimit} onChange={(event) => setCorpusLimit(Number(event.target.value))} /><p className="text-[11px] text-muted-foreground">优先保留样例对应的正例文档，其余按随机种子稳定补齐。{preset.id === "vectara-open-ragbench" && track === "TEXT_RAG" ? `设为 ${catalogCorpusSize(preset, split)} 才是官方完整主榜。` : `当前 Split 最多 ${catalogCorpusSize(preset, split)} 篇。`}</p></div>}
        <div className="rounded-md bg-muted/50 px-3 py-2 text-xs text-muted-foreground">准备结果会由系统自动归类到“{preset?.name || "所选数据集"}”，无需手动选择保存位置。</div>
        <Button type="submit" className="w-full" disabled={busy || !preset || sampleSize < 1 || corpusLimit < 1 || corpusLimit > catalogCorpusSize(preset, split) || (preset.id === "vectara-open-ragbench" && evidenceTypes.length === 0)}>{busy ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Download className="mr-2 h-4 w-4" />}后台准备数据集</Button>
      </form>
      {error && <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-xs text-destructive">{error}</div>}
      {recentImports.length > 0 && <div className="space-y-2 border-t pt-4"><div className="flex items-center justify-between gap-3"><Label>最近准备任务</Label><span className="text-[11px] text-muted-foreground">仅显示最近 5 条</span></div>{recentImports.map((job) => <div key={job.id} className="flex items-center justify-between gap-3 rounded-md border p-2 text-xs"><div className="min-w-0"><div className="flex items-center gap-2"><Badge variant="outline">{job.status}</Badge><span className="truncate">{catalog.find((item) => item.id === job.catalogId)?.name || job.catalogId} · v{job.targetVersion}</span></div><p className="mt-1 truncate text-muted-foreground">{job.errorMessage || importProgress(job)}</p></div>{(job.status === "QUEUED" || job.status === "RUNNING") && <Button size="icon" variant="ghost" onClick={() => void cancel(job.id)} title="取消"><X className="h-4 w-4" /></Button>}</div>)}</div>}
    </CardContent>
  </Card>;
}
