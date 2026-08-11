"use client";

import { useCallback, useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, FlaskConical } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  getRAGEvalCapabilities,
  getRAGEvalDataset,
  listRAGEvalDatasets,
  listRAGEvalProfiles,
  listRAGEvalRuns,
  type RAGEvalCapabilities,
  type RAGEvalDataset,
  type RAGEvalDatasetVersion,
  type RAGEvalProfile,
  type RAGEvalRun,
} from "@/lib/api";
import { DatasetsPanel } from "./components/datasets-panel";
import { RunList } from "./components/run-list";
import { RunWizard } from "./components/run-wizard";
import { nextRunPollDelay } from "./rag-eval-state";

export default function RAGEvaluationsPage() {
  const [capabilities, setCapabilities] = useState<RAGEvalCapabilities | null>(null);
  const [datasets, setDatasets] = useState<RAGEvalDataset[]>([]);
  const [versions, setVersions] = useState<RAGEvalDatasetVersion[]>([]);
  const [profiles, setProfiles] = useState<RAGEvalProfile[]>([]);
  const [runs, setRuns] = useState<RAGEvalRun[]>([]);
  const [error, setError] = useState("");

  const refreshCatalog = useCallback(async () => {
    try {
      const [capability, nextDatasets, nextProfiles] = await Promise.all([
        getRAGEvalCapabilities(), listRAGEvalDatasets(), listRAGEvalProfiles(),
      ]);
      const details = await Promise.all(nextDatasets.map((dataset) => getRAGEvalDataset(dataset.id)));
      setCapabilities(capability); setDatasets(nextDatasets); setProfiles(nextProfiles);
      setVersions(details.flatMap((detail) => detail.versions ?? [])); setError("");
    } catch (err) { setError(err instanceof Error ? err.message : "加载 RAG 测评目录失败"); }
  }, []);

  const refreshRuns = useCallback(async () => {
    try { setRuns(await listRAGEvalRuns()); setError(""); }
    catch (err) { setError(err instanceof Error ? err.message : "加载运行失败"); }
  }, []);

  useEffect(() => {
    const initialLoad = window.setTimeout(() => void Promise.all([refreshCatalog(), refreshRuns()]), 0);
    return () => window.clearTimeout(initialLoad);
  }, [refreshCatalog, refreshRuns]);
  useEffect(() => {
    const active = runs.some((run) => run.status === "QUEUED" || run.status === "RUNNING");
    const delay = nextRunPollDelay(active, document.hidden);
    if (delay === null) return;
    const timer = window.setTimeout(() => void refreshRuns(), delay);
    const onVisibility = () => { window.clearTimeout(timer); void refreshRuns(); };
    document.addEventListener("visibilitychange", onVisibility);
    return () => { window.clearTimeout(timer); document.removeEventListener("visibilitychange", onVisibility); };
  }, [runs, refreshRuns]);

  return <div className="mx-auto max-w-7xl space-y-6 p-6">
    <div className="flex items-start justify-between gap-4"><div><h1 className="flex items-center gap-2 text-2xl font-semibold"><FlaskConical className="h-6 w-6" />RAG 测评</h1><p className="mt-1 text-sm text-muted-foreground">固定数据集、真实检索/回答链路、基线对比与受控策略发布。</p></div><Button variant="outline" onClick={() => void Promise.all([refreshCatalog(), refreshRuns()])}>刷新</Button></div>
    {error && <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>}
    <div className="grid gap-4 md:grid-cols-3"><CapabilityCard title="功能开关" ok={capabilities?.enabled ?? false} detail={capabilities?.enabled ? "已启用" : "默认关闭"} /><CapabilityCard title="评分 Sidecar" ok={capabilities?.sidecarHealthy ?? false} detail={capabilities?.reason || "健康"} /><CapabilityCard title="指标包" ok={!!capabilities?.metricBundleVersion} detail={capabilities?.metricBundleVersion || "—"} /></div>
    <DatasetsPanel capabilities={capabilities} datasets={datasets} onChanged={refreshCatalog} />
    <RunWizard capabilities={capabilities} versions={versions} profiles={profiles} runs={runs} onCreated={refreshRuns} />
    <RunList runs={runs} onChanged={refreshRuns} />
  </div>;
}

function CapabilityCard({ title, ok, detail }: { title: string; ok: boolean; detail: string }) {
  return <Card><CardHeader className="pb-2"><CardDescription>{title}</CardDescription><CardTitle className="flex items-center gap-2 text-base">{ok ? <CheckCircle2 className="h-5 w-5 text-emerald-500" /> : <AlertTriangle className="h-5 w-5 text-amber-500" />}{detail}</CardTitle></CardHeader></Card>;
}
