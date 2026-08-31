"use client";

import { useEffect, useState } from "react";
import { Download, FileJson, FileText, Images } from "lucide-react";
import { ParserEvalMarkdown } from "@/components/rag-safe-render";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  getParserEvalArtifactText,
  parserEvalArtifactURL,
  type ParserEvalDocument,
  type ParserEvalEngine,
} from "@/lib/api";
import { formatParserEvalBytes, formatParserEvalDuration, formatParserEvalScore } from "../parser-eval-state";

export function EvidenceDialog({ runId, document, open, onOpenChange }: { runId: string; document: ParserEvalDocument | null; open: boolean; onOpenChange: (open: boolean) => void }) {
  const [tab, setTab] = useState("truth");
  const [markitdown, setMarkitdown] = useState<string>();
  const [anydoc, setAnydoc] = useState<string>();
  const [judgeRaw, setJudgeRaw] = useState<{ markitdown?: string; anydoc?: string }>();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    setTab("truth"); setMarkitdown(undefined); setAnydoc(undefined); setJudgeRaw(undefined); setError("");
  }, [document?.id, open]);

  useEffect(() => {
    if (!open || !document || tab === "truth") return;
    let cancelled = false;
    async function load() {
      setLoading(true); setError("");
      try {
        if (tab === "markitdown" && markitdown === undefined) {
          const value = await getParserEvalArtifactText(parserEvalArtifactURL(runId, document!.id, "markdown", { engine: "markitdown" }));
          if (!cancelled) setMarkitdown(value);
        }
        if (tab === "anydoc" && anydoc === undefined) {
          const value = await getParserEvalArtifactText(parserEvalArtifactURL(runId, document!.id, "markdown", { engine: "anydoc" }));
          if (!cancelled) setAnydoc(value);
        }
        if (tab === "judge" && judgeRaw === undefined) {
          const [first, second] = await Promise.allSettled([
            getParserEvalArtifactText(parserEvalArtifactURL(runId, document!.id, "judge-raw", { order: "markitdown-a" })),
            getParserEvalArtifactText(parserEvalArtifactURL(runId, document!.id, "judge-raw", { order: "anydoc-a" })),
          ]);
          if (first.status === "rejected" && second.status === "rejected") throw first.reason;
          if (!cancelled) {
            setJudgeRaw({
              markitdown: first.status === "fulfilled" ? prettyJSON(first.value) : undefined,
              anydoc: second.status === "fulfilled" ? prettyJSON(second.value) : undefined,
            });
            if (first.status === "rejected" || second.status === "rejected") setError("仅一个裁判位置产生了可用原始响应；另一位置失败或被跳过。");
          }
        }
      } catch (cause) {
        if (!cancelled) setError(cause instanceof Error ? cause.message : "加载证据失败");
      } finally {
        if (!cancelled) setLoading(false);
      }
    }
    void load();
    return () => { cancelled = true; };
  }, [open, document, tab, runId, markitdown, anydoc, judgeRaw]);

  if (!document) return null;
  const pages = document.truth.pages ?? [];
  return <Dialog open={open} onOpenChange={onOpenChange}>
    <DialogContent className="max-h-[92vh] sm:max-w-6xl">
      <DialogHeader><DialogTitle>{document.fileName}</DialogTitle><DialogDescription>页面真值、完整 Markdown、结构统计、warnings 与双位置裁判原始响应。内容通过受保护接口按需加载。</DialogDescription></DialogHeader>
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground"><span>{document.format.toUpperCase()}</span><span>·</span><span>{formatParserEvalBytes(document.sizeBytes)}</span><Button className="ml-auto" size="sm" variant="outline" render={<a href={parserEvalArtifactURL(runId, document.id, "source")} />}><Download className="mr-1 h-3.5 w-3.5" />下载原文</Button></div>
      <Tabs value={tab} onValueChange={setTab} className="min-h-0">
        <TabsList><TabsTrigger value="truth"><Images />页面真值</TabsTrigger><TabsTrigger value="markitdown"><FileText />MarkItDown</TabsTrigger><TabsTrigger value="anydoc"><FileText />AnyDoc</TabsTrigger><TabsTrigger value="judge"><FileJson />裁判原始响应</TabsTrigger></TabsList>
        {error && <p className="rounded-md bg-destructive/10 p-3 text-sm text-destructive">{error}</p>}
        {loading && <p className="py-4 text-center text-sm text-muted-foreground">正在加载证据…</p>}
        <TabsContent value="truth"><ScrollArea className="h-[64vh] pr-3"><div className="space-y-4"><p className="text-xs text-muted-foreground">源文档共 {document.truth.totalPages ?? "—"} 页，评分覆盖前 {document.truth.coveredPages ?? 0} 页 · renderer {formatParserEvalDuration(document.truth.renderDurationMs)}</p>{pages.length === 0 ? <p className="rounded-md border p-5 text-center text-muted-foreground">没有可用页面真值</p> : pages.map((page) => <figure key={page.page} className="overflow-hidden rounded-lg border bg-muted/20 p-2">{/* 证据 URL 依赖浏览器 session；Next 图片代理不会携带该管理员 cookie。 */}<img // eslint-disable-line @next/next/no-img-element
          className="mx-auto max-h-[56vh] bg-white object-contain" loading="lazy" alt={`${document.fileName} 第 ${page.page} 页`} src={parserEvalArtifactURL(runId, document.id, "truth-page", { page: page.page })} /><figcaption className="mt-2 text-center text-xs text-muted-foreground">第 {page.page} 页 · {page.width}×{page.height}</figcaption></figure>)}</div></ScrollArea></TabsContent>
        <TabsContent value="markitdown"><MarkdownEvidence document={document} engine="markitdown" markdown={markitdown} /></TabsContent>
        <TabsContent value="anydoc"><MarkdownEvidence document={document} engine="anydoc" markdown={anydoc} /></TabsContent>
        <TabsContent value="judge"><ScrollArea className="h-[64vh] pr-3"><div className="grid gap-4 lg:grid-cols-2"><JudgeRaw title="MarkItDown 作为 A" reason={document.judge.markitdownA?.verdict?.reason} raw={judgeRaw?.markitdown} /><JudgeRaw title="AnyDoc 作为 A" reason={document.judge.anydocA?.verdict?.reason} raw={judgeRaw?.anydoc} /></div></ScrollArea></TabsContent>
      </Tabs>
    </DialogContent>
  </Dialog>;
}

function MarkdownEvidence({ document, engine, markdown }: { document: ParserEvalDocument; engine: ParserEvalEngine; markdown?: string }) {
  const result = engine === "markitdown" ? document.markitdown : document.anydoc;
  const score = engine === "markitdown" ? document.judge.markitdownScore : document.judge.anydocScore;
  return <ScrollArea className="h-[64vh] pr-3"><div className="space-y-4">
    <div className="grid gap-2 rounded-md border p-3 text-xs sm:grid-cols-4"><span>纯解析 {formatParserEvalDuration(result.parseDurationMs)}</span><span>端到端 {formatParserEvalDuration(result.endToEndDurationMs)}</span><span>总分 {formatParserEvalScore(score?.total)}</span><span>Markdown {formatParserEvalBytes(result.markdown?.byteSize ?? 0)}</span></div>
    <Stats value={result.stats} />
    {(result.warnings?.length ?? 0) > 0 && <details className="rounded-md border p-3"><summary className="cursor-pointer text-sm">Warnings（{result.warnings?.length}）</summary><div className="mt-2 space-y-2">{result.warnings?.map((warning, index) => <p key={`${warning.code}-${index}`} className="text-xs"><strong>{warning.code}</strong> · {warning.message}{warning.degraded ? " · degraded" : ""}</p>)}</div></details>}
    <article className="prose prose-sm max-w-none rounded-md border bg-background p-4 dark:prose-invert">{markdown === undefined ? <p className="text-muted-foreground">选择此标签后加载完整 Markdown。</p> : <ParserEvalMarkdown>{markdown}</ParserEvalMarkdown>}</article>
  </div></ScrollArea>;
}

function Stats({ value }: { value: ParserEvalDocument["markitdown"]["stats"] }) {
  if (!value) return null;
  const entries = [["字符", value.characters], ["标题", value.headings], ["表格数据行", value.tableDataRows], ["列表项", value.listItems], ["链接", value.links], ["图片/资产标记", value.imageOrAssetMarkers], ["脚注", value.footnoteDefinitions]];
  return <div className="grid grid-cols-3 gap-2 rounded-md bg-muted/40 p-3 sm:grid-cols-7">{entries.map(([label, count]) => <div key={String(label)} className="text-center"><p className="font-mono text-sm">{count ?? 0}</p><p className="text-[11px] text-muted-foreground">{label}</p></div>)}</div>;
}

function JudgeRaw({ title, reason, raw }: { title: string; reason?: string; raw?: string }) {
  return <div className="min-w-0 rounded-md border p-3"><h3 className="font-medium">{title}</h3>{reason && <p className="mt-2 text-xs text-muted-foreground">裁判理由：{reason}</p>}<pre className="mt-3 max-h-[48vh] overflow-auto whitespace-pre-wrap break-all rounded bg-muted p-3 text-xs">{raw ?? "尚无原始响应"}</pre></div>;
}

function prettyJSON(raw: string): string {
  try { return JSON.stringify(JSON.parse(raw), null, 2); } catch { return raw; }
}
