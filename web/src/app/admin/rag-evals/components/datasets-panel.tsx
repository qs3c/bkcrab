"use client";

import { FormEvent, useMemo, useState } from "react";
import { AlertTriangle, FileCheck2, Loader2, Plus, Upload } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  createRAGEvalDataset,
  getRAGEvalDataset,
  getRAGEvalDatasetValidation,
  importRAGEvalDatasetVersion,
  type RAGEvalCapabilities,
  type RAGEvalDataset,
  type RAGEvalDatasetVersion,
  type RAGEvalValidationReport,
} from "@/lib/api";
import { validationIssueMessages } from "../rag-eval-state";

async function fileBase64(file: File): Promise<string> {
  const data = new Uint8Array(await file.arrayBuffer());
  let binary = "";
  for (let offset = 0; offset < data.length; offset += 0x8000) {
    binary += String.fromCharCode(...data.subarray(offset, offset + 0x8000));
  }
  return btoa(binary);
}

export function DatasetsPanel({ capabilities, datasets, onChanged }: {
  capabilities: RAGEvalCapabilities | null;
  datasets: RAGEvalDataset[];
  onChanged: () => Promise<void>;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [selected, setSelected] = useState("");
  const [versions, setVersions] = useState<RAGEvalDatasetVersion[]>([]);
  const [manifest, setManifest] = useState<File | null>(null);
  const [documents, setDocuments] = useState<File[]>([]);
  const [report, setReport] = useState<RAGEvalValidationReport | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const issues = useMemo(() => validationIssueMessages(report), [report]);

  async function loadDataset(id: string) {
    setSelected(id);
    setReport(null);
    if (!id) { setVersions([]); return; }
    try {
      const detail = await getRAGEvalDataset(id);
      setVersions(detail.versions ?? []);
    } catch (err) {
      setError(err instanceof Error ? err.message : "加载版本失败");
    }
  }

  async function createDataset(event: FormEvent) {
    event.preventDefault();
    if (!name.trim()) return;
    setBusy(true);
    try {
      const created = await createRAGEvalDataset(name.trim(), description.trim());
      setName(""); setDescription(""); setError("");
      await onChanged();
      await loadDataset(created.id);
    } catch (err) { setError(err instanceof Error ? err.message : "创建数据集失败"); }
    finally { setBusy(false); }
  }

  async function importVersion(event: FormEvent) {
    event.preventDefault();
    if (!selected || !manifest) return;
    setBusy(true); setReport(null);
    try {
      const parsed = JSON.parse(await manifest.text()) as { corpus?: Array<{ id?: string; fileName?: string }> };
      const byName = new Map(documents.map((file) => [file.name, file]));
      const uploads = await Promise.all((parsed.corpus ?? []).map(async (item) => {
        const file = byName.get(item.fileName ?? "");
        if (!file || !item.id) throw new Error(`缺少语料文件：${item.fileName || item.id || "unknown"}`);
        return { externalId: item.id, fileName: file.name, mediaType: file.type || "application/octet-stream", contentBase64: await fileBase64(file) };
      }));
      const nextVersion = Math.max(0, ...versions.map((item) => item.Version)) + 1;
      const result = await importRAGEvalDatasetVersion({ datasetId: selected, version: nextVersion, manifest: parsed, documents: uploads });
      setReport(result.report); setManifest(null); setDocuments([]); setError("");
      await loadDataset(selected);
    } catch (err) { setError(err instanceof Error ? err.message : "导入失败"); }
    finally { setBusy(false); }
  }

  async function inspectValidation(versionId: string) {
    try { setReport((await getRAGEvalDatasetValidation(versionId)).report); setError(""); }
    catch (err) { setError(err instanceof Error ? err.message : "读取校验报告失败"); }
  }

  return <div className="grid gap-6 xl:grid-cols-[360px_1fr]">
    <div className="space-y-6">
      <Card>
        <CardHeader><CardTitle>新建数据集</CardTitle><CardDescription>逻辑数据集可持续追加不可变版本。</CardDescription></CardHeader>
        <CardContent><form className="space-y-4" onSubmit={createDataset}>
          <div className="space-y-2"><Label htmlFor="dataset-name">名称</Label><Input id="dataset-name" value={name} onChange={(event) => setName(event.target.value)} maxLength={255} /></div>
          <div className="space-y-2"><Label htmlFor="dataset-description">说明</Label><Input id="dataset-description" value={description} onChange={(event) => setDescription(event.target.value)} maxLength={1000} /></div>
          <Button className="w-full" disabled={busy || !capabilities?.enabled || !name.trim()}>{busy ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Plus className="mr-2 h-4 w-4" />}创建</Button>
        </form></CardContent>
      </Card>
      <Card>
        <CardHeader><CardTitle>导入新版本</CardTitle><CardDescription>支持 capabilities 声明的 {capabilities?.importers?.join("、") || "canonical JSON"}；请求总量受服务端限制。</CardDescription></CardHeader>
        <CardContent><form className="space-y-4" onSubmit={importVersion}>
          <div className="space-y-2"><Label>数据集</Label><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={selected} onChange={(event) => void loadDataset(event.target.value)}><option value="">请选择</option>{datasets.map((dataset) => <option key={dataset.id} value={dataset.id}>{dataset.name}</option>)}</select></div>
          <div className="space-y-2"><Label htmlFor="manifest">manifest.json</Label><Input id="manifest" type="file" accept="application/json,.json" onChange={(event) => setManifest(event.target.files?.[0] ?? null)} /></div>
          <div className="space-y-2"><Label htmlFor="corpus-files">语料文件</Label><Input id="corpus-files" type="file" multiple onChange={(event) => setDocuments(Array.from(event.target.files ?? []))} /></div>
          <Button className="w-full" disabled={busy || !selected || !manifest}><Upload className="mr-2 h-4 w-4" />校验并导入</Button>
        </form></CardContent>
      </Card>
    </div>
    <div className="space-y-6">
      {error && <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>}
      <Card><CardHeader><CardTitle>版本与预览</CardTitle><CardDescription>首屏仅展示元数据；完整语料、contexts 与原因按需获取。</CardDescription></CardHeader><CardContent>
        <Table><TableHeader><TableRow><TableHead>版本</TableHead><TableHead>状态</TableHead><TableHead>文档</TableHead><TableHead>样例</TableHead><TableHead>大小</TableHead><TableHead /></TableRow></TableHeader><TableBody>
          {versions.length === 0 ? <TableRow><TableCell colSpan={6} className="text-center text-muted-foreground">选择数据集后查看版本</TableCell></TableRow> : versions.map((version) => <TableRow key={version.ID}><TableCell>v{version.Version}</TableCell><TableCell><Badge variant="outline">{version.Status}</Badge></TableCell><TableCell>{version.DocumentCount}</TableCell><TableCell>{version.CaseCount}</TableCell><TableCell>{(version.TotalBytes / 1024 / 1024).toFixed(1)} MB</TableCell><TableCell><Button size="sm" variant="ghost" onClick={() => void inspectValidation(version.ID)}><FileCheck2 className="mr-1 h-4 w-4" />校验报告</Button></TableCell></TableRow>)}
        </TableBody></Table>
      </CardContent></Card>
      {report && <Card><CardHeader><CardTitle className="flex items-center gap-2">{report.valid === false ? <AlertTriangle className="h-5 w-5 text-amber-500" /> : <FileCheck2 className="h-5 w-5 text-emerald-500" />}校验报告</CardTitle></CardHeader><CardContent>{issues.length === 0 ? <p className="text-sm text-muted-foreground">未发现错误或警告。</p> : <ul className="space-y-2 text-sm">{issues.map((issue, index) => <li key={`${issue}-${index}`} className="rounded bg-muted p-2 font-mono text-xs">{issue}</li>)}</ul>}</CardContent></Card>}
    </div>
  </div>;
}
