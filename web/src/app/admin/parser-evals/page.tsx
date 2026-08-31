"use client";

import { useCallback, useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, RefreshCw, ScanText } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { getParserEvalCapabilities, listParserEvalRuns, type ParserEvalCapabilities, type ParserEvalRun } from "@/lib/api";
import { CreateRunPanel } from "./components/create-run-panel";
import { RunList } from "./components/run-list";
import { nextParserEvalPollDelay, parserEvalRunIsActive } from "./parser-eval-state";

export default function ParserEvaluationsPage() {
  const [capabilities, setCapabilities] = useState<ParserEvalCapabilities | null>(null);
  const [runs, setRuns] = useState<ParserEvalRun[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try {
      const [nextCapabilities, page] = await Promise.all([getParserEvalCapabilities(), listParserEvalRuns()]);
      setCapabilities(nextCapabilities);
      setRuns(page.items ?? []);
      setError("");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "加载解析测评失败");
    } finally {
      setLoading(false);
    }
  }, []);

  const refreshRuns = useCallback(async () => {
    try { setRuns((await listParserEvalRuns()).items ?? []); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "刷新解析测评任务失败"); }
  }, []);

  useEffect(() => { const timer = window.setTimeout(() => void refresh(), 0); return () => window.clearTimeout(timer); }, [refresh]);
  useEffect(() => {
    const delay = nextParserEvalPollDelay(runs.some((run) => parserEvalRunIsActive(run.status)), document.hidden);
    if (delay === null) return;
    const timer = window.setTimeout(() => void refreshRuns(), delay);
    const visible = () => { if (!document.hidden) void refreshRuns(); };
    document.addEventListener("visibilitychange", visible);
    return () => { window.clearTimeout(timer); document.removeEventListener("visibilitychange", visible); };
  }, [runs, refreshRuns]);

  if (loading && !capabilities) return <div className="flex h-full items-center justify-center"><RefreshCw className="h-7 w-7 animate-spin text-muted-foreground" /></div>;

  return <div className="mx-auto max-w-7xl space-y-6 p-6">
    <div className="flex items-start justify-between gap-4"><div><h1 className="flex items-center gap-2 text-2xl font-semibold"><ScanText className="h-6 w-6" />解析测评</h1><p className="mt-1 text-sm text-muted-foreground">对用户上传的 DOCX、PPTX、XLSX 比较 MarkItDown 与 AnyDoc 的解析延迟和视觉质量。</p></div><Button variant="outline" onClick={() => void refresh()}><RefreshCw className="mr-1 h-4 w-4" />刷新</Button></div>
    {error && <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>}
    {capabilities && <>
      <CapabilityBanner capabilities={capabilities} />
      <div className="grid gap-4 md:grid-cols-3">
        <HealthCard title="页面 Renderer" healthy={capabilities.renderer.health.healthy} detail={capabilities.renderer.health.reason || capabilities.renderer.descriptor.libreOfficeVersion || "健康"} />
        {capabilities.parsers.map((parser) => <HealthCard key={parser.engine} title={parser.engine === "markitdown" ? "MarkItDown" : "AnyDoc"} healthy={parser.health.healthy} detail={parser.health.reason || `${parser.descriptor.name} ${parser.descriptor.version}`} />)}
      </div>
      <CreateRunPanel key={capabilities.judgeModelBindings.map((item) => item.id).join("|")} capabilities={capabilities} onCreated={refreshRuns} />
    </>}
    <RunList runs={runs} onChanged={refreshRuns} />
  </div>;
}

function CapabilityBanner({ capabilities }: { capabilities: ParserEvalCapabilities }) {
  const healthy = capabilities.enabled && capabilities.available;
  return <Card className={healthy ? "border-emerald-500/30" : "border-amber-500/40"}><CardContent className="flex items-start gap-3 pt-5">{healthy ? <CheckCircle2 className="mt-0.5 h-5 w-5 text-emerald-500" /> : <AlertTriangle className="mt-0.5 h-5 w-5 text-amber-500" />}<div><p className="font-medium">{healthy ? "解析测评运行时可用" : capabilities.enabled ? "解析测评暂不可启动" : "解析测评未启用"}</p><p className="mt-1 text-sm text-muted-foreground">{healthy ? `${capabilities.judgeModelBindings.length} 个视觉裁判模型可选；页面最多覆盖前 ${capabilities.maxPages} 页，${capabilities.renderDPI} DPI。` : capabilityReason(capabilities.reason)}</p></div></CardContent></Card>;
}

function HealthCard({ title, healthy, detail }: { title: string; healthy: boolean; detail: string }) {
  return <Card><CardHeader className="pb-3"><CardDescription>{title}</CardDescription><CardTitle className="flex items-center gap-2 text-base">{healthy ? <CheckCircle2 className="h-4 w-4 text-emerald-500" /> : <AlertTriangle className="h-4 w-4 text-amber-500" />}{detail}</CardTitle></CardHeader></Card>;
}

function capabilityReason(reason?: string): string {
  return ({ disabled: "请在部署配置中启用 PARSER_EVAL_ENABLED。", worker_disabled: "API 已启用，但当前实例没有启动 worker。", renderer_unhealthy: "页面 renderer 健康检查失败。", parser_unhealthy: "至少一个解析器健康检查失败或不支持全部 Office 格式。", no_vision_judge_model: "请配置至少一个支持图片输入的完整模型绑定。", runtime_unavailable: "解析测评运行时尚未装配完成。" } as Record<string, string>)[reason || ""] || reason || "依赖健康状态尚未就绪。";
}
