"use client";

import { FormEvent, useEffect, useState } from "react";
import { RotateCcw, Save, ShieldCheck } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { createRAGEvalProfile, getRAGCapabilities, getRAGPolicies, promoteRAGIngestionPolicy, promoteRAGRuntimePolicy, rollbackRAGRuntimePolicy, type RAGCapabilities, type RAGEvalProfile, type RAGEvalRun, type RAGParserEngine, type RAGPolicyAuditDTO, type RAGPolicyRecordDTO } from "@/lib/api";
import { profileOptionLabel } from "../rag-eval-state";
import { promotionGateReasons } from "../result-state";

const defaultProfile = JSON.stringify({
  ingestion: { version: 0, chunkSize: 512, chunkOverlap: 64, parseMode: "standard", enrichmentEnabled: false, documentAI: {}, embedding: { contractFingerprint: "", model: "", dims: 1024 } },
  runtime: { version: 0, topN: 5, candidateTopK: 20, minScore: 0.2, temperature: 0.1, maxTokens: 1024, ragPromptBundleVersion: "rag-answer-v1" },
  rewriteEnabled: false, hydeEnabled: false, rerankerEnabled: false, rerankerFailurePolicy: "fallback_rrf", answerModel: "provider/model",
}, null, 2);

const runtimeWhitelist = ["topN", "candidateTopK", "minScore", "temperature", "maxTokens", "ragPromptBundleVersion"];

function selectedParserEngine(value: string): RAGParserEngine | "" {
  try {
    const profile = JSON.parse(value) as { ingestion?: { parserEngine?: unknown } };
    return profile.ingestion?.parserEngine === "anydoc" || profile.ingestion?.parserEngine === "markitdown" ? profile.ingestion.parserEngine : "";
  } catch { return ""; }
}

function withParserEngine(value: string, parserEngine: RAGParserEngine): string {
  const profile = JSON.parse(value) as { ingestion?: Record<string, unknown> };
  profile.ingestion = { ...(profile.ingestion ?? {}), parserEngine };
  return JSON.stringify(profile, null, 2);
}

export function ProfilePolicyPanel({ profiles, runs, onProfileChanged }: { profiles: RAGEvalProfile[]; runs: RAGEvalRun[]; onProfileChanged: () => Promise<void> }) {
  const [profileName, setProfileName] = useState("");
  const [profileJSON, setProfileJSON] = useState(defaultProfile);
  const [sourceProfileId, setSourceProfileId] = useState("");
  const [ragCapabilities, setRAGCapabilities] = useState<RAGCapabilities | null>(null);
  const [runId, setRunId] = useState("");
  const [profileId, setProfileId] = useState("");
  const [confirmationRunId, setConfirmationRunId] = useState("");
  const [note, setNote] = useState("");
  const [fields, setFields] = useState(runtimeWhitelist);
  const [runtime, setRuntime] = useState<RAGPolicyRecordDTO | undefined>();
  const [audits, setAudits] = useState<RAGPolicyAuditDTO[]>([]);
  const [error, setError] = useState("");
  const selectedRun = runs.find((item) => item.id === runId);
  const reasons = promotionGateReasons({ runStatus: selectedRun?.status, runId, confirmationRunId, note });

  async function refreshPolicies() {
    try {
      const [policies, capabilities] = await Promise.all([getRAGPolicies(), getRAGCapabilities()]);
      setRuntime(policies.runtime?.active); setRAGCapabilities(capabilities);
      setAudits([...(policies.runtime?.audit ?? []), ...(policies.ingestion?.audit ?? [])].sort((a, b) => String(b.CreatedAt).localeCompare(String(a.CreatedAt))));
      setProfileJSON((current) => selectedParserEngine(current) ? current : withParserEngine(current, capabilities.defaultParserEngine));
    }
    catch (err) { setError(err instanceof Error ? err.message : "读取策略失败"); }
  }
  useEffect(() => { const timer = window.setTimeout(() => void refreshPolicies(), 0); return () => window.clearTimeout(timer); }, []);

  async function saveProfile(event: FormEvent) {
    event.preventDefault();
    try { await createRAGEvalProfile(profileName.trim(), JSON.parse(profileJSON)); setProfileName(""); setError(""); await onProfileChanged(); }
    catch (err) { setError(err instanceof Error ? err.message : "保存 Profile 失败"); }
  }

  function loadProfile(id: string) {
    setSourceProfileId(id);
    const profile = profiles.find((item) => item.id === id);
    if (!profile) return;
    setProfileJSON(profile.profileJson);
    setProfileName(`${profile.name} · 副本`);
    setError("");
  }

  function chooseParser(parserEngine: RAGParserEngine) {
    try { setProfileJSON((current) => withParserEngine(current, parserEngine)); setError(""); }
    catch { setError("Profile JSON 无法解析，请先修复 JSON 后再选择解析器"); }
  }

  async function publish(kind: "runtime" | "ingestion") {
    if (reasons.length) { setError(reasons.join("；")); return; }
    const label = kind === "runtime" ? "在线 RuntimePolicy" : "整库 IngestionPolicy";
    if (!window.confirm(`确认发布 ${label}？\n候选：${runId}\n复验：${confirmationRunId}\n备注：${note}`)) return;
    try {
      if (kind === "runtime") await promoteRAGRuntimePolicy({ runId, profileId, confirmationRunId, fields, note });
      else await promoteRAGIngestionPolicy({ runId, profileId, confirmationRunId, note });
      setError(""); await refreshPolicies();
    } catch (err) { setError(err instanceof Error ? err.message : "发布失败"); }
  }

  async function rollback(targetVersion: number) {
    if (!runtime || !note.trim()) { setError("回滚必须填写审计备注"); return; }
    if (!window.confirm(`确认从 runtime v${runtime.Version} 回滚到 v${targetVersion}？\n备注：${note}`)) return;
    try { await rollbackRAGRuntimePolicy({ expectedVersion: runtime.Version, targetVersion, note }); setError(""); await refreshPolicies(); }
    catch (err) { setError(err instanceof Error ? err.message : "回滚失败"); }
  }

  return <div className="grid gap-6 xl:grid-cols-2">
    <Card><CardHeader><CardTitle>实验 Profile</CardTitle><CardDescription>从已有 Profile 创建副本，分别固定 AnyDoc/MarkItDown；版本保存后不可变，运行结果会记录建库耗时。</CardDescription></CardHeader><CardContent><form className="space-y-3" onSubmit={saveProfile}>
      <Label htmlFor="source-profile">基于已有 Profile</Label><select id="source-profile" className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={sourceProfileId} onChange={(event) => loadProfile(event.target.value)}><option value="">从空白模板创建</option>{profiles.map((item) => <option key={item.id} value={item.id}>{profileOptionLabel(item, profiles)}</option>)}</select>
      <Label>文档解析器</Label><div className="grid gap-2 sm:grid-cols-2">{ragCapabilities?.parsers.map((parser) => <Button key={parser.engine} type="button" variant={selectedParserEngine(profileJSON) === parser.engine ? "default" : "outline"} disabled={!parser.available} title={parser.reason || parser.supportedExtensions.join(", ")} onClick={() => chooseParser(parser.engine)}>{parser.label || parser.engine}{!parser.available ? "（不可用）" : ""}</Button>) ?? <p className="text-xs text-muted-foreground">正在读取解析器能力…</p>}</div>
      <p className="text-xs text-muted-foreground">当前选择：{selectedParserEngine(profileJSON) || "未选择"}。不同解析器进入独立生成/解析缓存，便于公平比较耗时。</p>
      <Label htmlFor="profile-name">名称</Label><Input id="profile-name" value={profileName} onChange={(event) => setProfileName(event.target.value)} /><Label htmlFor="profile-json">完整实验参数 JSON</Label><Textarea id="profile-json" className="min-h-96 font-mono text-xs" value={profileJSON} onChange={(event) => setProfileJSON(event.target.value)} /><Button type="submit" disabled={!profileName.trim() || !selectedParserEngine(profileJSON)}><Save className="mr-2 h-4 w-4" />保存不可变 Profile</Button>
    </form></CardContent></Card>
    <div className="space-y-6"><Card><CardHeader><CardTitle className="flex items-center gap-2"><ShieldCheck className="h-5 w-5" />策略发布门禁</CardTitle><CardDescription>发布面只允许白名单字段；未勾选的实验差异明确保持未发布。</CardDescription></CardHeader><CardContent className="space-y-4">
      <div className="grid gap-3 sm:grid-cols-2"><Field label="候选成功运行"><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={runId} onChange={(event) => { setRunId(event.target.value); const run = runs.find((item) => item.id === event.target.value); if (run) setProfileId(run.profileId); }}><option value="">请选择</option>{runs.map((item) => <option key={item.id} value={item.id}>{item.id} · {item.status}</option>)}</select></Field><Field label="独立 confirmation run"><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={confirmationRunId} onChange={(event) => setConfirmationRunId(event.target.value)}><option value="">请选择</option>{runs.filter((item) => item.status === "SUCCEEDED").map((item) => <option key={item.id} value={item.id}>{item.id}</option>)}</select></Field></div>
      <Field label="Profile"><select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={profileId} onChange={(event) => setProfileId(event.target.value)}><option value="">请选择</option>{profiles.map((item) => <option key={item.id} value={item.id}>{profileOptionLabel(item, profiles)}</option>)}</select></Field>
      <div><Label>Runtime 发布白名单</Label><div className="mt-2 flex flex-wrap gap-2">{runtimeWhitelist.map((field) => <Button key={field} type="button" size="sm" variant={fields.includes(field) ? "default" : "outline"} onClick={() => setFields((value) => value.includes(field) ? value.filter((item) => item !== field) : [...value, field])}>{field}</Button>)}</div><p className="mt-2 text-xs text-muted-foreground">索引/embedding/解析差异不会随 Runtime 发布；它们仅可通过 IngestionPolicy 发布。</p></div>
      <Field label="审计备注（必填）"><Textarea value={note} onChange={(event) => setNote(event.target.value)} /></Field>
      {reasons.length > 0 && <div className="rounded bg-amber-500/10 p-3 text-xs text-amber-700">{reasons.map((reason) => <div key={reason}>• {reason}</div>)}</div>}
      {error && <p className="text-sm text-destructive">{error}</p>}
      <div className="flex flex-wrap gap-2"><Button type="button" disabled={reasons.length > 0 || fields.length === 0} onClick={() => void publish("runtime")}>发布 Runtime</Button><Button type="button" variant="outline" disabled={reasons.length > 0} onClick={() => void publish("ingestion")}>发布 Ingestion</Button></div>
    </CardContent></Card>
    <Card><CardHeader><CardTitle>发布 / 回滚审计</CardTitle><CardDescription>当前 Runtime v{runtime?.Version ?? "—"}；每次操作都保留操作者、来源运行和备注。</CardDescription></CardHeader><CardContent className="space-y-2">{audits.length === 0 ? <p className="text-sm text-muted-foreground">暂无审计记录</p> : audits.map((audit) => <div key={audit.ID} className="flex items-center justify-between rounded border p-3 text-sm"><div><Badge variant="outline">{audit.Action}</Badge><span className="ml-2">v{audit.FromVersion} → v{audit.ToVersion}</span><p className="mt-1 text-xs text-muted-foreground">{audit.Note || "无备注"} · {audit.ActorID}</p></div>{audit.Action === "PUBLISH" && audit.FromVersion > 0 && runtime && <Button size="sm" variant="ghost" onClick={() => void rollback(audit.FromVersion)}><RotateCcw className="mr-1 h-4 w-4" />回滚</Button>}</div>)}</CardContent></Card></div>
  </div>;
}

function Field({ label, children }: { label: string; children: React.ReactNode }) { return <div className="space-y-2"><Label>{label}</Label>{children}</div>; }
