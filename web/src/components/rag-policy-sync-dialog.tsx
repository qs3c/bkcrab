"use client";

import * as React from "react";
import { AlertTriangle, Loader2, RotateCcw, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { cancelKnowledgeBasePolicySync, rollbackKnowledgeBasePolicy, startKnowledgeBasePolicySync, RAGRequestError, type KnowledgeBase, type RAGKBPolicyStatus } from "@/lib/api";
import { policyVisualState, requiresKBNameConfirmation, safePolicySyncFailure } from "./rag-policy-sync-state";

function bytes(value?: number) { if (!value) return "0 B"; const units = ["B", "KB", "MB", "GB", "TB"]; const index = Math.min(units.length - 1, Math.floor(Math.log(value) / Math.log(1024))); return `${(value / 1024 ** index).toFixed(1)} ${units[index]}`; }
function duration(value?: number) { if (value === undefined) return "服务未提供"; if (value < 60) return `${value} 秒`; return `约 ${Math.ceil(value / 60)} 分钟`; }
function parseJSON(value?: string): Record<string, unknown> { try { return value ? JSON.parse(value) : {}; } catch { return {}; } }

export function RAGPolicySyncDialog({ open, onOpenChange, kb, status, readOnly, onRefresh }: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  kb: KnowledgeBase | null;
  status: RAGKBPolicyStatus | null;
  readOnly: boolean;
  onRefresh: () => Promise<void>;
}) {
  const [confirmed, setConfirmed] = React.useState(false);
  const [typedName, setTypedName] = React.useState("");
  const [note, setNote] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const visual = policyVisualState(status);
  const requiresName = requiresKBNameConfirmation(status);
  const progress = parseJSON(status?.syncTask?.progressJson);

  React.useEffect(() => {
    if (!open || visual !== "syncing") return;
    const timer = window.setTimeout(() => void onRefresh(), document.hidden ? 10_000 : 2_000);
    return () => window.clearTimeout(timer);
  }, [open, visual, status?.syncTask?.progressJson, onRefresh]);

  function handleError(err: unknown) {
    if (err instanceof RAGRequestError && (err.status === 409 || err.status === 423)) setError("另一项同步或写操作正在进行，请刷新状态后重试。");
    else setError(err instanceof Error ? err.message : "策略操作失败");
  }

  async function start() {
    if (!kb || !status || !confirmed || (requiresName && typedName !== kb.name)) return;
    setBusy(true);
    try { await startKnowledgeBasePolicySync(kb.id, status.latestVersion); setError(""); await onRefresh(); }
    catch (err) { handleError(err); }
    finally { setBusy(false); }
  }

  async function cancel() {
    if (!kb || !status?.syncTask) return;
    setBusy(true);
    try { await cancelKnowledgeBasePolicySync(kb.id, status.syncTask.id); setError(""); await onRefresh(); }
    catch (err) { handleError(err); }
    finally { setBusy(false); }
  }

  async function rollback() {
    if (!kb?.activeGenerationId || !status?.syncTask?.sourceGenerationId || !note.trim()) { setError("回滚必须填写审计备注，且旧 generation 仍在回滚窗口内。"); return; }
    if (!window.confirm(`确认将 ${kb.name} 回滚到同步前 generation？\n备注：${note}`)) return;
    setBusy(true);
    try { await rollbackKnowledgeBasePolicy(kb.id, { targetGenerationId: status.syncTask.sourceGenerationId, expectedGenerationId: kb.activeGenerationId, note }); setError(""); await onRefresh(); }
    catch (err) { handleError(err); }
    finally { setBusy(false); }
  }

  const estimate = status?.estimate;
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent className="max-h-[90vh] max-w-3xl overflow-y-auto"><DialogHeader><DialogTitle>知识库 IngestionPolicy 同步</DialogTitle><DialogDescription>重新索引整个知识库，构建期间旧索引继续回答；完整校验后才原子切换。</DialogDescription></DialogHeader>
    {!status || !kb ? <div className="flex justify-center p-8"><Loader2 className="h-6 w-6 animate-spin" /></div> : <div className="space-y-5">
      <div className="flex items-center gap-2"><Badge variant="outline">当前 v{status.pinnedVersion}</Badge><span>→</span><Badge>最新 v{status.latestVersion}</Badge>{visual === "syncing" && <Badge variant="outline">同步中</Badge>}</div>
      {status.fullCollectionRebuild && <div className="rounded-md border border-amber-500/30 bg-amber-500/10 p-3 text-sm text-amber-700"><AlertTriangle className="mr-2 inline h-4 w-4" />Embedding 合约发生变化，将创建新的向量 collection；旧 collection 在切换前保持服务。</div>}
      <div><h3 className="text-sm font-medium">策略差异</h3><div className="mt-2 divide-y rounded-md border">{status.differences.map((item) => <div key={item.field} className="grid grid-cols-[1fr_1fr_auto_1fr] gap-2 p-2 font-mono text-xs"><span>{item.field}</span><span className="truncate">{JSON.stringify(item.from)}</span><span>→</span><span className="truncate">{JSON.stringify(item.to)}</span></div>)}</div></div>
      <div><h3 className="text-sm font-medium">整库重建估算</h3><div className="mt-2 grid gap-2 rounded-md border bg-muted/30 p-3 text-sm sm:grid-cols-3"><span>文档：{estimate?.documentCount ?? 0}</span><span>源数据：{bytes(estimate?.sourceBytes)}</span><span>页数：{estimate?.pageCountEstimate ?? "服务未提供"}</span><span>时间：{duration(estimate?.durationSecEstimate)}</span><span>费用：{estimate?.costUsdEstimate === undefined ? "服务未提供" : `$${estimate.costUsdEstimate.toFixed(4)}`}</span><span>临时存储上限：{bytes(estimate?.temporaryBytesMax)}</span></div></div>
      {visual === "syncing" && <div className="rounded-md border p-3"><p className="font-medium">同步进度</p><pre className="mt-2 overflow-auto text-xs">{JSON.stringify(progress, null, 2)}</pre><p className="mt-2 text-xs text-muted-foreground">同步期间上传、删除和单文档 reindex 已锁定；读取和问答继续使用旧索引。</p></div>}
      {visual === "failed" && <div className="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">{safePolicySyncFailure()} {status.syncTask?.errorCode && <span className="font-mono">({status.syncTask.errorCode})</span>}</div>}
      {visual === "outdated" && <div className="space-y-3"><label className="flex items-start gap-2 text-sm"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />我确认将重建整个知识库；期间旧索引继续回答，完成前不会切换生产读取。</label>{requiresName && <div className="space-y-2"><Label htmlFor="confirm-kb-name">超大知识库，请输入名称 “{kb.name}”</Label><Input id="confirm-kb-name" value={typedName} onChange={(event) => setTypedName(event.target.value)} /></div>}</div>}
      {status.syncTask?.rollbackAllowed && <div className="space-y-2 rounded-md border p-3"><Label>回滚审计备注</Label><Textarea value={note} onChange={(event) => setNote(event.target.value)} /><Button variant="outline" disabled={busy || readOnly || !note.trim()} onClick={() => void rollback()}><RotateCcw className="mr-2 h-4 w-4" />回滚到同步前索引</Button></div>}
      {error && <p className="text-sm text-destructive">{error}</p>}
    </div>}
    <DialogFooter>{visual === "syncing" && <Button variant="outline" disabled={busy || readOnly} onClick={() => void cancel()}><X className="mr-2 h-4 w-4" />取消同步</Button>}{visual === "outdated" && <Button disabled={busy || readOnly || !confirmed || (requiresName && typedName !== kb?.name)} onClick={() => void start()}>{busy && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}确认并开始整库同步</Button>}</DialogFooter>
  </DialogContent></Dialog>;
}
