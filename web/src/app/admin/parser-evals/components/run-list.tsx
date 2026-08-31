"use client";

import { useState } from "react";
import Link from "next/link";
import { ExternalLink, RotateCcw, Square, Trash2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  cancelParserEvalRun,
  deleteParserEvalRun,
  parserEvalIdempotencyKey,
  retryParserEvalRun,
  type ParserEvalRun,
} from "@/lib/api";
import { parserEvalCanRetry, parserEvalRunIsActive, parserEvalRunIsTerminal, parserEvalStageLabel } from "../parser-eval-state";

export function RunList({ runs, onChanged }: { runs: ParserEvalRun[]; onChanged: () => Promise<void> }) {
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");

  async function action(run: ParserEvalRun, kind: "cancel" | "retry" | "delete") {
    if (kind === "delete" && !window.confirm(`删除解析测评 ${run.id}？相关原文、页面、Markdown 和原始裁判响应会一并删除。`)) return;
    setBusy(`${kind}:${run.id}`);
    setError("");
    try {
      if (kind === "cancel") await cancelParserEvalRun(run.id, parserEvalIdempotencyKey());
      if (kind === "retry") await retryParserEvalRun(run.id, parserEvalIdempotencyKey());
      if (kind === "delete") await deleteParserEvalRun(run.id);
      await onChanged();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "运行操作失败");
    } finally {
      setBusy("");
    }
  }

  return <Card>
    <CardHeader><CardTitle>解析测评任务</CardTitle><CardDescription>任务全局串行执行；刷新或服务重启后会从已持久化的文档阶段继续。</CardDescription></CardHeader>
    <CardContent>
      {error && <p className="mb-3 rounded-md bg-destructive/10 p-3 text-sm text-destructive">{error}</p>}
      <Table><TableHeader><TableRow><TableHead>任务</TableHead><TableHead>裁判模型</TableHead><TableHead>阶段</TableHead><TableHead>进度</TableHead><TableHead>状态</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader>
        <TableBody>{runs.length === 0 ? <TableRow><TableCell colSpan={6} className="py-8 text-center text-muted-foreground">暂无解析测评任务</TableCell></TableRow> : runs.map((run) => {
          const total = run.progress?.documentsTotal ?? 0;
          const completed = run.progress?.documentsCompleted ?? 0;
          const judge = run.executionSnapshot?.judge?.modelDisplayName || run.executionSnapshot?.judge?.model || run.executionSnapshot?.judgeModelBindingId || "—";
          return <TableRow key={run.id}>
            <TableCell><Link className="inline-flex items-center gap-1 font-mono text-xs hover:underline" href={`/admin/parser-evals/${encodeURIComponent(run.id)}/`}>{run.id}<ExternalLink className="h-3 w-3" /></Link><p className="mt-1 text-xs text-muted-foreground">{new Date(run.createdAt).toLocaleString("zh-CN")}</p></TableCell>
            <TableCell className="max-w-56 truncate text-sm" title={judge}>{judge}</TableCell>
            <TableCell>{parserEvalStageLabel(run.stage)}</TableCell>
            <TableCell><span className="font-mono text-xs">{completed}/{total}</span>{total > 0 && <div className="mt-1 h-1.5 w-28 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${Math.min(100, completed / total * 100)}%` }} /></div>}</TableCell>
            <TableCell><Badge variant="outline">{run.status}</Badge>{run.cancelRequestedAt && !parserEvalRunIsTerminal(run.status) && <p className="mt-1 text-[11px] text-amber-600">正在取消</p>}</TableCell>
            <TableCell className="text-right"><div className="flex justify-end gap-1">
              {parserEvalRunIsActive(run.status) && <Button size="sm" variant="ghost" disabled={!!busy} onClick={() => void action(run, "cancel")}><Square className="mr-1 h-3.5 w-3.5" />取消</Button>}
              {parserEvalCanRetry(run.status) && <Button size="sm" variant="ghost" disabled={!!busy} onClick={() => void action(run, "retry")}><RotateCcw className="mr-1 h-3.5 w-3.5" />重试失败项</Button>}
              {(run.status === "DRAFT" || parserEvalRunIsTerminal(run.status)) && <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" disabled={!!busy} onClick={() => void action(run, "delete")}><Trash2 className="mr-1 h-3.5 w-3.5" />删除</Button>}
            </div></TableCell>
          </TableRow>;
        })}</TableBody>
      </Table>
    </CardContent>
  </Card>;
}
