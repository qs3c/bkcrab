"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  getRAGEvalDataset,
  type RAGEvalCapabilities,
  type RAGEvalDataset,
  type RAGEvalDatasetVersion,
} from "@/lib/api";
import { CatalogImportPanel } from "./catalog-import-panel";

function logicalDatasetName(name: string): string {
  return name.replace(/\s*·\s*(TEXT_RAG|PDF_E2E)\s*$/u, "").trim();
}

function createdAtLabel(value: string): string {
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "—";
  return parsed.toLocaleString("zh-CN", { hour12: false });
}

export function DatasetsPanel({ capabilities, datasets, onChanged }: {
  capabilities: RAGEvalCapabilities | null;
  datasets: RAGEvalDataset[];
  onChanged: () => Promise<void>;
}) {
  const [selected, setSelected] = useState("");
  const [versions, setVersions] = useState<RAGEvalDatasetVersion[]>([]);
  const [error, setError] = useState("");
  const datasetGroups = useMemo(() => {
    const grouped = new Map<string, { key: string; label: string; ids: string[] }>();
    for (const dataset of datasets) {
      const label = logicalDatasetName(dataset.name);
      const current = grouped.get(label);
      if (current) current.ids.push(dataset.id);
      else grouped.set(label, { key: label, label, ids: [dataset.id] });
    }
    return [...grouped.values()];
  }, [datasets]);

  const loadDataset = useCallback(async (keyOrID: string) => {
    const group = datasetGroups.find((item) => item.key === keyOrID || item.ids.includes(keyOrID));
    setSelected(group?.key || keyOrID);
    const ids = group?.ids || (keyOrID ? [keyOrID] : []);
    if (ids.length === 0) {
      setVersions([]);
      return;
    }
    try {
      const details = await Promise.all(ids.map((id) => getRAGEvalDataset(id)));
      setVersions(details.flatMap((detail) => detail.versions ?? []).sort((left, right) =>
        right.CreatedAt.localeCompare(left.CreatedAt) || right.Version - left.Version));
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "加载数据集版本失败");
    }
  }, [datasetGroups]);

  useEffect(() => {
    if (datasetGroups.length === 0 || datasetGroups.some((group) => group.key === selected)) return;
    const timer = window.setTimeout(() => void loadDataset(datasetGroups[0].key), 0);
    return () => window.clearTimeout(timer);
  }, [datasetGroups, loadDataset, selected]);

  const handleCatalogChanged = useCallback(async () => {
    await onChanged();
    if (selected) await loadDataset(selected);
  }, [loadDataset, onChanged, selected]);

  return <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1.35fr)_minmax(380px,0.65fr)]">
    <CatalogImportPanel
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
              {datasetGroups.length === 0 && <option value="">尚无已准备数据集</option>}
              {datasetGroups.map((group) => <option key={group.key} value={group.key}>{group.label}</option>)}
            </select>
          </div>
          <div className="overflow-x-auto">
            <Table>
              <TableHeader><TableRow><TableHead>版本</TableHead><TableHead>状态</TableHead><TableHead>文档 / 样例</TableHead><TableHead className="text-right">创建时间</TableHead></TableRow></TableHeader>
              <TableBody>
                {versions.length === 0 ? <TableRow><TableCell colSpan={4} className="py-8 text-center text-muted-foreground">{selected ? "这个数据集还没有准备好的版本" : "先在左侧准备数据集"}</TableCell></TableRow> : versions.map((version) => <TableRow key={version.ID}>
                  <TableCell className="font-medium">v{version.Version}</TableCell>
                  <TableCell><Badge variant="outline">{version.Status}</Badge></TableCell>
                  <TableCell>{version.DocumentCount} / {version.CaseCount}</TableCell>
                  <TableCell className="whitespace-nowrap text-right text-xs text-muted-foreground">{createdAtLabel(version.CreatedAt)}</TableCell>
                </TableRow>)}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>
    </div>
  </div>;
}
