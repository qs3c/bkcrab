"use client";

import { FormEvent, useCallback, useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, FlaskConical, Loader2, Plus } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  cancelRAGEvalRun,
  createRAGEvalDataset,
  getRAGEvalCapabilities,
  listRAGEvalDatasets,
  listRAGEvalRuns,
  type RAGEvalCapabilities,
  type RAGEvalDataset,
  type RAGEvalRun,
} from "@/lib/api";

export default function RAGEvaluationsPage() {
  const [capabilities, setCapabilities] = useState<RAGEvalCapabilities | null>(null);
  const [datasets, setDatasets] = useState<RAGEvalDataset[]>([]);
  const [runs, setRuns] = useState<RAGEvalRun[]>([]);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try {
      const capability = await getRAGEvalCapabilities();
      setCapabilities(capability);
      const [nextDatasets, nextRuns] = await Promise.all([
        listRAGEvalDatasets(),
        listRAGEvalRuns(),
      ]);
      setDatasets(nextDatasets);
      setRuns(nextRuns);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "加载 RAG 测评数据失败");
    }
  }, []);

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(), 5000);
    return () => window.clearInterval(timer);
  }, [refresh]);

  async function createDataset(event: FormEvent) {
    event.preventDefault();
    if (!name.trim()) return;
    setBusy(true);
    try {
      await createRAGEvalDataset(name.trim(), description.trim());
      setName("");
      setDescription("");
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "创建数据集失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="mx-auto max-w-7xl space-y-6 p-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-semibold"><FlaskConical className="h-6 w-6" />RAG 测评</h1>
          <p className="mt-1 text-sm text-muted-foreground">固定数据集、真实检索/回答链路、基线对比与受控策略发布。</p>
        </div>
        <Button variant="outline" onClick={() => void refresh()}>刷新</Button>
      </div>

      {error && <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>}

      <div className="grid gap-4 md:grid-cols-3">
        <CapabilityCard title="功能开关" ok={capabilities?.enabled ?? false} detail={capabilities?.enabled ? "已启用" : "默认关闭"} />
        <CapabilityCard title="评分 Sidecar" ok={capabilities?.sidecarHealthy ?? false} detail={capabilities?.reason || "健康"} />
        <CapabilityCard title="指标包" ok={!!capabilities?.metricBundleVersion} detail={capabilities?.metricBundleVersion || "—"} />
      </div>

      <div className="grid gap-6 lg:grid-cols-[360px_1fr]">
        <Card>
          <CardHeader><CardTitle>新建数据集</CardTitle><CardDescription>数据版本发布后不可变；语料与样例通过后续版本导入。</CardDescription></CardHeader>
          <CardContent>
            <form className="space-y-4" onSubmit={createDataset}>
              <div className="space-y-2"><Label htmlFor="dataset-name">名称</Label><Input id="dataset-name" value={name} onChange={(event) => setName(event.target.value)} maxLength={255} /></div>
              <div className="space-y-2"><Label htmlFor="dataset-description">说明</Label><Input id="dataset-description" value={description} onChange={(event) => setDescription(event.target.value)} maxLength={1000} /></div>
              <Button className="w-full" disabled={busy || !capabilities?.enabled || !name.trim()}>{busy ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Plus className="mr-2 h-4 w-4" />}创建</Button>
            </form>
          </CardContent>
        </Card>

        <Card>
          <CardHeader><CardTitle>数据集</CardTitle><CardDescription>逻辑数据集及其不可变版本入口。</CardDescription></CardHeader>
          <CardContent>
            <Table><TableHeader><TableRow><TableHead>名称</TableHead><TableHead>创建时间</TableHead><TableHead>说明</TableHead></TableRow></TableHeader><TableBody>
              {datasets.length === 0 ? <TableRow><TableCell colSpan={3} className="text-center text-muted-foreground">暂无数据集</TableCell></TableRow> : datasets.map((dataset) => <TableRow key={dataset.id}><TableCell className="font-medium">{dataset.name}</TableCell><TableCell>{new Date(dataset.createdAt).toLocaleString()}</TableCell><TableCell className="max-w-md truncate">{dataset.description || "—"}</TableCell></TableRow>)}
            </TableBody></Table>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader><CardTitle>运行</CardTitle><CardDescription>完成、取消或失败后自动停止轮询；skipped/error 不计入评分分母。</CardDescription></CardHeader>
        <CardContent><Table><TableHeader><TableRow><TableHead>运行</TableHead><TableHead>模式</TableHead><TableHead>阶段</TableHead><TableHead>状态</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader><TableBody>
          {runs.length === 0 ? <TableRow><TableCell colSpan={5} className="text-center text-muted-foreground">暂无运行</TableCell></TableRow> : runs.map((run) => <TableRow key={run.id}><TableCell className="font-mono text-xs">{run.id}</TableCell><TableCell>{run.mode === "FULL_PIPELINE" ? "完整 Pipeline" : "仅在线 Pipeline"}</TableCell><TableCell>{run.stage}</TableCell><TableCell><Badge variant="outline">{run.status}</Badge></TableCell><TableCell className="text-right"><Button size="sm" variant="ghost" disabled={!['QUEUED','RUNNING'].includes(run.status)} onClick={() => void cancelRAGEvalRun(run.id).then(refresh)}>取消</Button></TableCell></TableRow>)}
        </TableBody></Table></CardContent>
      </Card>
    </div>
  );
}

function CapabilityCard({ title, ok, detail }: { title: string; ok: boolean; detail: string }) {
  return <Card><CardHeader className="pb-2"><CardDescription>{title}</CardDescription><CardTitle className="flex items-center gap-2 text-base">{ok ? <CheckCircle2 className="h-5 w-5 text-emerald-500" /> : <AlertTriangle className="h-5 w-5 text-amber-500" />}{detail}</CardTitle></CardHeader></Card>;
}
