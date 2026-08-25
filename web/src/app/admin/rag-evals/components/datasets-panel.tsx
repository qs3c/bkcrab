"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { AlertTriangle, FileCheck2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  getRAGEvalDataset,
  getRAGEvalDatasetValidation,
  type RAGEvalCapabilities,
  type RAGEvalDataset,
  type RAGEvalDatasetVersion,
  type RAGEvalValidationReport,
} from "@/lib/api";
import { validationIssueMessages } from "../rag-eval-state";
import { CatalogImportPanel } from "./catalog-import-panel";

export function DatasetsPanel({ capabilities, datasets, onChanged }: {
  capabilities: RAGEvalCapabilities | null;
  datasets: RAGEvalDataset[];
  onChanged: () => Promise<void>;
}) {
  const [selected, setSelected] = useState("");
  const [versions, setVersions] = useState<RAGEvalDatasetVersion[]>([]);
  const [report, setReport] = useState<RAGEvalValidationReport | null>(null);
  const [error, setError] = useState("");
  const issues = useMemo(() => validationIssueMessages(report), [report]);

  const loadDataset = useCallback(async (id: string) => {
    setSelected(id);
    setReport(null);
    if (!id) {
      setVersions([]);
      return;
    }
    try {
      const detail = await getRAGEvalDataset(id);
      setVersions(detail.versions ?? []);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "加载数据集版本失败");
    }
  }, []);

  useEffect(() => {
    if (selected || datasets.length === 0) return;
    const timer = window.setTimeout(() => void loadDataset(datasets[0].id), 0);
    return () => window.clearTimeout(timer);
  }, [datasets, loadDataset, selected]);

  const handleCatalogChanged = useCallback(async () => {
    await onChanged();
    if (selected) await loadDataset(selected);
  }, [loadDataset, onChanged, selected]);

  async function inspectValidation(versionId: string) {
    try {
      setReport((await getRAGEvalDatasetValidation(versionId)).report);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "读取校验报告失败");
    }
  }

  return <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1.35fr)_minmax(380px,0.65fr)]">
    <CatalogImportPanel
      datasets={datasets}
      maxRunCases={capabilities?.maxRunCases || 0}
      onChanged={handleCatalogChanged}
      onDatasetSelected={(id) => void loadDataset(id)}
    />
    <div className="space-y-6">
      {error && <div className="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>}
      <Card>
        <CardHeader>
          <CardTitle>已准备的数据集版本</CardTitle>
          <CardDescription>每次准备成功都会冻结一个不可变版本；创建测评运行时选择这里的版本。</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="prepared-dataset">查看数据集</Label>
            <select id="prepared-dataset" className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={selected} onChange={(event) => void loadDataset(event.target.value)}>
              {datasets.length === 0 && <option value="">尚无已准备数据集</option>}
              {datasets.map((dataset) => <option key={dataset.id} value={dataset.id}>{dataset.name}</option>)}
            </select>
          </div>
          <div className="overflow-x-auto">
            <Table>
              <TableHeader><TableRow><TableHead>版本</TableHead><TableHead>状态</TableHead><TableHead>文档 / 样例</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader>
              <TableBody>
                {versions.length === 0 ? <TableRow><TableCell colSpan={4} className="py-8 text-center text-muted-foreground">{selected ? "这个数据集还没有准备好的版本" : "先在左侧准备数据集"}</TableCell></TableRow> : versions.map((version) => <TableRow key={version.ID}>
                  <TableCell className="font-medium">v{version.Version}</TableCell>
                  <TableCell><Badge variant="outline">{version.Status}</Badge></TableCell>
                  <TableCell>{version.DocumentCount} / {version.CaseCount}</TableCell>
                  <TableCell className="text-right"><Button size="sm" variant="ghost" onClick={() => void inspectValidation(version.ID)}><FileCheck2 className="mr-1 h-4 w-4" />校验</Button></TableCell>
                </TableRow>)}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>
      {report && <Card>
        <CardHeader><CardTitle className="flex items-center gap-2">{report.valid === false ? <AlertTriangle className="h-5 w-5 text-amber-500" /> : <FileCheck2 className="h-5 w-5 text-emerald-500" />}校验报告</CardTitle></CardHeader>
        <CardContent>{issues.length === 0 ? <p className="text-sm text-muted-foreground">未发现错误或警告。</p> : <ul className="space-y-2 text-sm">{issues.map((issue, index) => <li key={`${issue}-${index}`} className="rounded bg-muted p-2 font-mono text-xs">{issue}</li>)}</ul>}</CardContent>
      </Card>}
    </div>
  </div>;
}
