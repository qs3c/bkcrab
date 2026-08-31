"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { ArrowLeft, RefreshCw, RotateCcw, Square, Trash2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  cancelParserEvalRun,
  deleteParserEvalRun,
  getParserEvalRun,
  parserEvalIdempotencyKey,
  retryParserEvalRun,
  type ParserEvalRunDetail,
} from "@/lib/api";
import { DocumentResults } from "../components/document-results";
import { RunSummary } from "../components/run-summary";
import { nextParserEvalPollDelay, parserEvalCanRetry, parserEvalRunIsActive, parserEvalRunIsTerminal, parserEvalStageLabel } from "../parser-eval-state";

export default function ParserEvaluationDetailPage() {
  const params = useParams<{ id: string }>();
  const router = useRouter();
  const runId = decodeURIComponent(params.id);
  const [detail, setDetail] = useState<ParserEvalRunDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try { setDetail(await getParserEvalRun(runId)); setError(""); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "加载解析测评详情失败"); }
    finally { setLoading(false); }
  }, [runId]);

  useEffect(() => { const timer = window.setTimeout(() => void refresh(), 0); return () => window.clearTimeout(timer); }, [refresh]);
  useEffect(() => {
    const delay = nextParserEvalPollDelay(!!detail && parserEvalRunIsActive(detail.run.status), document.hidden);
    if (delay === null) return;
    const timer = window.setTimeout(() => void refresh(), delay);
    return () => window.clearTimeout(timer);
  }, [detail, refresh]);

  async function action(kind: "cancel" | "retry" | "delete") {
    if (!detail) return;
    if (kind === "delete" && !window.confirm("删除此解析测评及全部证据文件？")) return;
    setBusy(true); setError("");
    try {
      if (kind === "cancel") await cancelParserEvalRun(runId, parserEvalIdempotencyKey());
      if (kind === "retry") await retryParserEvalRun(runId, parserEvalIdempotencyKey());
      if (kind === "delete") { await deleteParserEvalRun(runId); router.push("/admin/parser-evals/"); return; }
      await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "任务操作失败"); }
    finally { setBusy(false); }
  }

  if (loading && !detail) return <div className="flex h-full items-center justify-center"><RefreshCw className="h-7 w-7 animate-spin text-muted-foreground" /></div>;

  return <div className="mx-auto max-w-7xl space-y-6 p-6">
    <div><Link href="/admin/parser-evals/" className="inline-flex items-center text-sm text-muted-foreground hover:text-foreground"><ArrowLeft className="mr-1 h-4 w-4" />返回解析测评</Link></div>
    {error && <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>}
    {detail && <>
      <div className="flex flex-wrap items-start justify-between gap-4"><div><div className="flex items-center gap-3"><h1 className="font-mono text-xl font-semibold">{detail.run.id}</h1><Badge variant="outline">{detail.run.status}</Badge></div><p className="mt-2 text-sm text-muted-foreground">{parserEvalStageLabel(detail.run.stage)} · {detail.run.progress.documentsCompleted ?? 0}/{detail.run.progress.documentsTotal ?? detail.documents.length} 文档 · 裁判 {detail.run.executionSnapshot.judge?.modelDisplayName || detail.run.executionSnapshot.judge?.model || detail.run.executionSnapshot.judgeModelBindingId || "—"}</p></div><div className="flex gap-2"><Button variant="outline" disabled={busy} onClick={() => void refresh()}><RefreshCw className="mr-1 h-4 w-4" />刷新</Button>{parserEvalRunIsActive(detail.run.status) && <Button variant="outline" disabled={busy} onClick={() => void action("cancel")}><Square className="mr-1 h-4 w-4" />取消</Button>}{parserEvalCanRetry(detail.run.status) && <Button variant="outline" disabled={busy} onClick={() => void action("retry")}><RotateCcw className="mr-1 h-4 w-4" />重试失败项</Button>}{(detail.run.status === "DRAFT" || parserEvalRunIsTerminal(detail.run.status)) && <Button variant="destructive" disabled={busy} onClick={() => void action("delete")}><Trash2 className="mr-1 h-4 w-4" />删除</Button>}</div></div>
      {detail.run.cancelRequestedAt && !parserEvalRunIsTerminal(detail.run.status) && <Card className="border-amber-500/40"><CardContent className="pt-5 text-sm text-amber-700">取消请求已记录；当前不可中断步骤结束后任务会转为 CANCELLED。</CardContent></Card>}
      {detail.run.errorMessage && <Card className="border-destructive/40"><CardContent className="pt-5 text-sm text-destructive"><strong>{detail.run.errorCode || "运行错误"}</strong><p className="mt-1">{detail.run.errorMessage}</p></CardContent></Card>}
      {detail.run.status === "DRAFT" && <Card className="border-amber-500/40"><CardContent className="pt-5 text-sm text-amber-700">这是未启动草稿。若浏览器上传队列已丢失，请删除后重新创建任务。</CardContent></Card>}
      <RunSummary summary={detail.summary ?? detail.run.summary ?? {}} />
      <DocumentResults runId={detail.run.id} documents={detail.documents} />
    </>}
  </div>;
}
