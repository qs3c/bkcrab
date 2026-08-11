"use client";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cancelRAGEvalRun, type RAGEvalRun } from "@/lib/api";

export function RunList({ runs, onChanged }: { runs: RAGEvalRun[]; onChanged: () => Promise<void> }) {
  return <Card><CardHeader><CardTitle>运行队列</CardTitle><CardDescription>进度来自 durable run；终态自动停止轮询，详细 contexts 和 reasons 不在首屏加载。</CardDescription></CardHeader><CardContent><Table><TableHeader><TableRow><TableHead>运行</TableHead><TableHead>模式</TableHead><TableHead>阶段/进度</TableHead><TableHead>状态</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader><TableBody>
    {runs.length === 0 ? <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">暂无运行</TableCell></TableRow> : runs.map((run) => <TableRow key={run.id}><TableCell className="font-mono text-xs">{run.id}</TableCell><TableCell>{run.mode === "FULL_PIPELINE" ? "完整 Pipeline" : "仅在线"}</TableCell><TableCell><div>{run.stage}</div><div className="max-w-64 truncate font-mono text-xs text-muted-foreground">{run.progressJson || "{}"}</div></TableCell><TableCell><Badge variant="outline">{run.status}</Badge></TableCell><TableCell className="text-right"><Button size="sm" variant="ghost" disabled={!['QUEUED','RUNNING'].includes(run.status)} onClick={() => void cancelRAGEvalRun(run.id).then(onChanged)}>取消</Button></TableCell></TableRow>)}
  </TableBody></Table></CardContent></Card>;
}
