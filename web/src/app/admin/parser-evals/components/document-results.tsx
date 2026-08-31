"use client";

import { useState } from "react";
import { Eye } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { ParserEvalDocument } from "@/lib/api";
import { formatParserEvalDuration, formatParserEvalScore, parserEvalWinnerLabel } from "../parser-eval-state";
import { EvidenceDialog } from "./evidence-dialog";

export function DocumentResults({ runId, documents }: { runId: string; documents: ParserEvalDocument[] }) {
  const [selected, setSelected] = useState<ParserEvalDocument | null>(null);
  return <Card><CardHeader><CardTitle>文档结果</CardTitle><CardDescription>纯解析耗时排除上传和 bundle 传输；端到端耗时包含完整 parser sidecar 调用。</CardDescription></CardHeader><CardContent>
    <Table><TableHeader><TableRow><TableHead>文档</TableHead><TableHead>MarkItDown 延迟</TableHead><TableHead>AnyDoc 延迟</TableHead><TableHead>质量总分</TableHead><TableHead>胜者</TableHead><TableHead>状态</TableHead><TableHead className="text-right">证据</TableHead></TableRow></TableHeader><TableBody>{documents.map((document) => <TableRow key={document.id}>
      <TableCell><p className="max-w-64 truncate font-medium" title={document.fileName}>{document.fileName}</p><p className="text-xs text-muted-foreground">{document.format.toUpperCase()} · #{document.ordinal}</p></TableCell>
      <TableCell><p>{formatParserEvalDuration(document.markitdown.parseDurationMs)}</p><p className="text-xs text-muted-foreground">端到端 {formatParserEvalDuration(document.markitdown.endToEndDurationMs)}</p></TableCell>
      <TableCell><p>{formatParserEvalDuration(document.anydoc.parseDurationMs)}</p><p className="text-xs text-muted-foreground">端到端 {formatParserEvalDuration(document.anydoc.endToEndDurationMs)}</p></TableCell>
      <TableCell><p>{formatParserEvalScore(document.judge.markitdownScore?.total)} / {formatParserEvalScore(document.judge.anydocScore?.total)}</p><p className="text-xs text-muted-foreground">MarkItDown / AnyDoc</p></TableCell>
      <TableCell>{parserEvalWinnerLabel(document.judge.winner)}</TableCell>
      <TableCell><Badge variant="outline">{document.status}</Badge>{(document.errorMessage || document.markitdown.error?.message || document.anydoc.error?.message || document.judge.error?.message) && <p className="mt-1 max-w-52 truncate text-xs text-destructive" title={document.errorMessage || document.markitdown.error?.message || document.anydoc.error?.message || document.judge.error?.message}>{document.errorMessage || document.markitdown.error?.message || document.anydoc.error?.message || document.judge.error?.message}</p>}</TableCell>
      <TableCell className="text-right"><Button size="sm" variant="outline" onClick={() => setSelected(document)}><Eye className="mr-1 h-3.5 w-3.5" />查看</Button></TableCell>
    </TableRow>)}</TableBody></Table>
    <EvidenceDialog runId={runId} document={selected} open={!!selected} onOpenChange={(open) => { if (!open) setSelected(null); }} />
  </CardContent></Card>;
}
