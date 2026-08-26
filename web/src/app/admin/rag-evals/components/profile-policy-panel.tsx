"use client";

import { FormEvent, useEffect, useMemo, useState } from "react";
import { Database, Gauge, RotateCcw, Save, ShieldCheck, Trash2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  createRAGEvalProfile,
  deleteRAGEvalProfile,
  getRAGCapabilities,
  getRAGPolicies,
  promoteRAGIngestionPolicy,
  promoteRAGRuntimePolicy,
  rollbackRAGRuntimePolicy,
  type RAGCapabilities,
  type RAGEvalProfile,
  type RAGEvalRun,
  type RAGParserEngine,
  type RAGPolicyAuditDTO,
  type RAGPolicyRecordDTO,
  type RAGSparseAnalyzer,
} from "@/lib/api";
import { isProfileDeletionPending, profileOptionLabel } from "../rag-eval-state";
import { promotionGateReasons } from "../result-state";

interface IngestionDraft {
  version?: number;
  chunkSize?: number;
  chunkOverlap?: number;
  sparseAnalyzer?: RAGSparseAnalyzer;
  parserEngine?: RAGParserEngine;
  [key: string]: unknown;
}

interface RuntimeDraft {
  version?: number;
  topN?: number;
  candidateTopK?: number;
  minScore?: number;
  temperature?: number;
  maxTokens?: number;
  ragPromptBundleVersion?: string;
  [key: string]: unknown;
}

interface ProfileDraft {
  ingestion?: IngestionDraft;
  runtime?: RuntimeDraft;
  rewriteEnabled?: boolean;
  hydeEnabled?: boolean;
  rerankerEnabled?: boolean;
  answerModel?: string;
  [key: string]: unknown;
}

const defaultProfile: ProfileDraft = {
  ingestion: {
    version: 0,
    chunkSize: 512,
    chunkOverlap: 64,
    sparseAnalyzer: "chinese",
    parseMode: "standard",
    enrichmentEnabled: false,
    documentAI: {},
    embedding: { contractFingerprint: "", model: "", dims: 1024 },
  },
  runtime: {
    version: 0,
    topN: 5,
    candidateTopK: 20,
    minScore: 0.2,
    temperature: 0.1,
    maxTokens: 1024,
    ragPromptBundleVersion: "rag-answer-v1",
  },
  rewriteEnabled: false,
  hydeEnabled: false,
  rerankerEnabled: false,
  rerankerTimeoutMs: 5000,
  rerankerFailurePolicy: "fallback_rrf",
  answerModel: "provider/model",
};

const runtimeWhitelist = ["topN", "candidateTopK", "minScore", "temperature", "maxTokens", "ragPromptBundleVersion"];
const runtimeFieldLabels: Record<string, string> = {
  topN: "最终返回条数",
  candidateTopK: "候选召回数",
  minScore: "最低相关分",
  temperature: "回答温度",
  maxTokens: "回答长度",
  ragPromptBundleVersion: "提示词版本",
};

const sparseAnalyzerOptions: Array<{ value: RAGSparseAnalyzer; label: string; description: string }> = [
  { value: "chinese", label: "中文 RAG", description: "Jieba 中文分词" },
  { value: "english", label: "English RAG", description: "英文词干与停用词" },
  { value: "multilingual", label: "中英混合 RAG", description: "ICU Unicode 分词" },
];

function parseProfile(value: string): ProfileDraft | null {
  try {
    const parsed = JSON.parse(value) as unknown;
    return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed as ProfileDraft : null;
  } catch {
    return null;
  }
}

function stringifyProfile(profile: ProfileDraft): string {
  return JSON.stringify(profile, null, 2);
}

function selectedParserEngine(profile: ProfileDraft | null): RAGParserEngine | "" {
  const parser = profile?.ingestion?.parserEngine;
  return parser === "anydoc" || parser === "markitdown" ? parser : "";
}

function selectedSparseAnalyzer(profile: ProfileDraft | null): RAGSparseAnalyzer {
  const analyzer = profile?.ingestion?.sparseAnalyzer;
  return analyzer === "english" || analyzer === "multilingual" ? analyzer : "chinese";
}

function numberValue(value: unknown, fallback: number): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

function parserReason(reason: string): string {
  const labels: Record<string, string> = {
    office_disabled: "Office 文档解析功能未开启",
    parser_sidecar_not_configured: "解析服务地址未配置",
    parser_health_unavailable: "尚未取得解析服务健康状态",
    parser_health_stale: "解析服务健康状态已过期",
    parser_protocol_mismatch: "解析服务协议版本不匹配",
    office_capability_unavailable: "解析服务未声明兼容的 Office 能力",
  };
  return labels[reason] || reason || "解析服务当前不可用";
}

function decodePolicy(record: RAGPolicyRecordDTO | undefined, fallback: Record<string, unknown>): Record<string, unknown> {
  if (!record?.PolicyJSON) return fallback;
  try {
    const parsed = JSON.parse(record.PolicyJSON) as unknown;
    return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed as Record<string, unknown> : fallback;
  } catch {
    return fallback;
  }
}

function systemProfileJSON(
  policies: Record<"runtime" | "ingestion", { active?: RAGPolicyRecordDTO }>,
  parser: RAGParserEngine,
  seed: ProfileDraft = defaultProfile,
): string {
  const profile = structuredClone(seed);
  profile.ingestion = decodePolicy(policies.ingestion?.active, profile.ingestion ?? {}) as IngestionDraft;
  profile.runtime = decodePolicy(policies.runtime?.active, profile.runtime ?? {}) as RuntimeDraft;
  profile.ingestion.parserEngine = parser;
  return stringifyProfile(profile);
}

function runModeLabel(mode: RAGEvalRun["mode"]): string {
  return mode === "FULL_PIPELINE" ? "完整 Pipeline" : "仅在线复验";
}

export function ProfilePolicyPanel({ profiles, runs, onProfileChanged, section = "all" }: {
  profiles: RAGEvalProfile[];
  runs: RAGEvalRun[];
  onProfileChanged: () => Promise<void>;
  section?: "profile" | "policy" | "all";
}) {
  const [profileName, setProfileName] = useState("新实验 Profile");
  const [profileJSON, setProfileJSON] = useState(() => stringifyProfile(defaultProfile));
  const [baseProfileJSON, setBaseProfileJSON] = useState(() => stringifyProfile(defaultProfile));
  const [sourceProfileId, setSourceProfileId] = useState("");
  const [profileDirty, setProfileDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [deletingProfileId, setDeletingProfileId] = useState("");
  const [ragCapabilities, setRAGCapabilities] = useState<RAGCapabilities | null>(null);
  const [publishKind, setPublishKind] = useState<"runtime" | "ingestion">("runtime");
  const [runId, setRunId] = useState("");
  const [confirmationRunId, setConfirmationRunId] = useState("");
  const [note, setNote] = useState("");
  const [fields, setFields] = useState(runtimeWhitelist);
  const [runtime, setRuntime] = useState<RAGPolicyRecordDTO | undefined>();
  const [ingestion, setIngestion] = useState<RAGPolicyRecordDTO | undefined>();
  const [audits, setAudits] = useState<RAGPolicyAuditDTO[]>([]);
  const [profileError, setProfileError] = useState("");
  const [policyError, setPolicyError] = useState("");
  const [profileMessage, setProfileMessage] = useState("");
  const [policyMessage, setPolicyMessage] = useState("");
  const draft = useMemo(() => parseProfile(profileJSON), [profileJSON]);
  const orderedProfiles = useMemo(() => [...profiles].sort((left, right) => {
    const byCreatedAt = right.createdAt.localeCompare(left.createdAt);
    return byCreatedAt || right.id.localeCompare(left.id);
  }), [profiles]);
  const sourceProfile = profiles.find((item) => item.id === sourceProfileId);
  const sourceProfileRuns = runs.filter((item) => item.profileId === sourceProfileId);
  const profileDeletionPending = isProfileDeletionPending(deletingProfileId, sourceProfileId);
  const selectedRun = runs.find((item) => item.id === runId);
  const selectedProfile = profiles.find((item) => item.id === selectedRun?.profileId);

  const candidateRuns = useMemo(() => runs.filter((run) => run.status === "SUCCEEDED" && !!run.indexGenerationId &&
    (publishKind === "runtime" || run.mode === "FULL_PIPELINE"))
    .sort((left, right) => String(right.createdAt).localeCompare(String(left.createdAt))), [publishKind, runs]);
  const confirmationRuns = useMemo(() => {
    if (!selectedRun) return [];
    return runs.filter((run) => run.status === "SUCCEEDED" && run.id !== selectedRun.id &&
      run.profileId === selectedRun.profileId && run.datasetVersionId === selectedRun.datasetVersionId &&
      (publishKind === "runtime"
        ? run.mode === "ONLINE_ONLY" && run.indexGenerationId === selectedRun.indexGenerationId
        : run.mode === "FULL_PIPELINE" && !!run.indexGenerationId))
      .sort((left, right) => String(right.createdAt).localeCompare(String(left.createdAt)));
  }, [publishKind, runs, selectedRun]);

  const reasons = promotionGateReasons({
    runStatus: selectedRun?.status,
    runId,
    confirmationRunId,
    note,
  });
  if (!selectedRun?.profileId) reasons.push("候选运行没有可用的 Profile");
  if (confirmationRunId && !confirmationRuns.some((run) => run.id === confirmationRunId)) reasons.push("复验运行与候选运行不兼容");
  if (publishKind === "runtime" && fields.length === 0) reasons.push("至少选择一个要发布的 Runtime 参数");

  async function refreshPolicies(resetProfile = false) {
    try {
      const [policies, capabilities] = await Promise.all([getRAGPolicies(), getRAGCapabilities()]);
      setRuntime(policies.runtime?.active);
      setIngestion(policies.ingestion?.active);
      setRAGCapabilities(capabilities);
      setAudits([...(policies.runtime?.audit ?? []), ...(policies.ingestion?.audit ?? [])]
        .sort((left, right) => String(right.CreatedAt).localeCompare(String(left.CreatedAt))));
      const nextBase = systemProfileJSON(policies, capabilities.defaultParserEngine);
      setBaseProfileJSON(nextBase);
      if (resetProfile && !profileDirty && !sourceProfileId) setProfileJSON(nextBase);
      setPolicyError("");
    } catch (err) {
      setPolicyError(err instanceof Error ? err.message : "读取策略失败");
    }
  }

  useEffect(() => {
    const timer = window.setTimeout(() => void refreshPolicies(true), 0);
    return () => window.clearTimeout(timer);
    // Initial capability/policy snapshot only; later refreshes are explicit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (profileDirty || sourceProfileId || !ragCapabilities || !runtime || !ingestion) return;
    const systemProfile = profiles.find((profile) => profile.name === "系统默认全功能");
    const seed = systemProfile ? parseProfile(systemProfile.profileJson) : null;
    if (!seed) return;
    const timer = window.setTimeout(() => {
      const nextBase = systemProfileJSON({ runtime: { active: runtime }, ingestion: { active: ingestion } }, ragCapabilities.defaultParserEngine, seed);
      setBaseProfileJSON(nextBase);
      setProfileJSON(nextBase);
    }, 0);
    return () => window.clearTimeout(timer);
  }, [ingestion, profileDirty, profiles, ragCapabilities, runtime, sourceProfileId]);

  function changeProfile(mutator: (profile: ProfileDraft) => void) {
    const next = parseProfile(profileJSON);
    if (!next) {
      setProfileError("高级 JSON 格式有误，请先修复后再调整参数");
      return;
    }
    mutator(next);
    setProfileJSON(stringifyProfile(next));
    setProfileDirty(true);
    setProfileError("");
    setProfileMessage("");
  }

  function changeIngestion(field: string, value: unknown) {
    changeProfile((profile) => { profile.ingestion = { ...(profile.ingestion ?? {}), [field]: value }; });
  }

  function changeRuntime(field: string, value: unknown) {
    changeProfile((profile) => { profile.runtime = { ...(profile.runtime ?? {}), [field]: value }; });
  }

  function loadProfile(id: string) {
    setSourceProfileId(id);
    const profile = profiles.find((item) => item.id === id);
    if (profile) {
      setProfileJSON(profile.profileJson);
      setProfileName(`${profile.name} · 新实验`);
    } else {
      setProfileJSON(baseProfileJSON);
      setProfileName("新实验 Profile");
    }
    setProfileDirty(false);
    setProfileError("");
    setProfileMessage("");
  }

  async function saveProfile(event: FormEvent) {
    event.preventDefault();
    const parsed = parseProfile(profileJSON);
    if (!parsed) {
      setProfileError("Profile JSON 格式有误");
      return;
    }
    setSaving(true);
    try {
      await createRAGEvalProfile(profileName.trim(), parsed);
      setProfileError("");
      setProfileMessage("Profile 已保存，可在创建测评运行时直接选择。");
      setProfileDirty(false);
      await onProfileChanged();
    } catch (err) {
      setProfileError(err instanceof Error ? err.message : "保存 Profile 失败");
    } finally {
      setSaving(false);
    }
  }

  async function deleteSourceProfile() {
    if (!sourceProfile || sourceProfileRuns.length > 0) return;
    const createdAt = new Date(sourceProfile.createdAt);
    const createdLabel = Number.isNaN(createdAt.getTime()) ? sourceProfile.createdAt : createdAt.toLocaleString("zh-CN", { hour12: false });
    const systemNote = sourceProfile.id.startsWith("rep_system_") ? "\n这是系统自动生成的 Profile；如果它仍对应当前默认策略，服务重启时可能再次生成。" : "";
    if (!window.confirm(`确认删除 Profile“${sourceProfile.name}”？\n创建时间：${createdLabel}\n\n删除后无法恢复。${systemNote}`)) return;
    setDeletingProfileId(sourceProfile.id);
    setProfileError("");
    setProfileMessage("");
    try {
      await deleteRAGEvalProfile(sourceProfile.id);
      setSourceProfileId("");
      setProfileJSON(baseProfileJSON);
      setProfileName("新实验 Profile");
      setProfileDirty(false);
      await onProfileChanged();
      setProfileMessage(`Profile“${sourceProfile.name}”已删除。`);
    } catch (err) {
      setProfileError(err instanceof Error ? err.message : "删除 Profile 失败");
    } finally {
      setDeletingProfileId("");
    }
  }

  function choosePublishKind(kind: "runtime" | "ingestion") {
    setPublishKind(kind);
    setRunId("");
    setConfirmationRunId("");
    setPolicyError("");
    setPolicyMessage("");
  }

  async function publish() {
    if (reasons.length || !selectedRun) {
      setPolicyError(reasons.join("；"));
      return;
    }
    const label = publishKind === "runtime" ? "Runtime（在线参数）" : "Ingestion（建库参数）";
    if (!window.confirm(`确认发布 ${label}？\n候选：${runId}\n复验：${confirmationRunId}\n备注：${note}`)) return;
    try {
      if (publishKind === "runtime") {
        await promoteRAGRuntimePolicy({ runId, profileId: selectedRun.profileId, confirmationRunId, fields, note });
      } else {
        await promoteRAGIngestionPolicy({ runId, profileId: selectedRun.profileId, confirmationRunId, note });
      }
      setPolicyError("");
      setPolicyMessage(publishKind === "runtime"
        ? "Runtime 已发布，新检索/回答请求会读取新参数；多实例部署最多约 5 秒完成同步。"
        : "Ingestion 已发布，新知识库会采用新参数；已有知识库不会自动重建。"
      );
      await refreshPolicies();
    } catch (err) {
      setPolicyError(err instanceof Error ? err.message : "发布失败");
    }
  }

  async function rollback(targetVersion: number) {
    if (!runtime || !note.trim()) {
      setPolicyError("回滚必须填写审计备注");
      return;
    }
    if (!window.confirm(`确认从 Runtime v${runtime.Version} 回滚到 v${targetVersion}？\n备注：${note}`)) return;
    try {
      await rollbackRAGRuntimePolicy({ expectedVersion: runtime.Version, targetVersion, note });
      setPolicyError("");
      setPolicyMessage(`Runtime 已回滚到 v${targetVersion}。`);
      await refreshPolicies();
    } catch (err) {
      setPolicyError(err instanceof Error ? err.message : "回滚失败");
    }
  }

  return <div className="space-y-6">
    {section !== "policy" && <Card>
      <CardHeader>
        <CardTitle>实验 Profile</CardTitle>
        <CardDescription>直接用表单创建实验配置；不需要自己编写 JSON。保存后配置不可变，便于复现实验结果。</CardDescription>
      </CardHeader>
      <CardContent>
        <form className="space-y-6" onSubmit={saveProfile}>
          <div className="grid gap-4 lg:grid-cols-2">
            <Field label="基于已有配置">
              <div className="flex gap-2">
                <select className="h-9 min-w-0 flex-1 rounded-md border bg-background px-3 text-sm" value={sourceProfileId} onChange={(event) => loadProfile(event.target.value)}>
                  <option value="">系统当前默认</option>
                  {orderedProfiles.map((profile) => <option key={profile.id} value={profile.id}>{profileOptionLabel(profile, profiles)}</option>)}
                </select>
                <Button type="button" variant="outline" className="shrink-0 text-destructive hover:text-destructive" disabled={!sourceProfile || sourceProfileRuns.length > 0 || profileDeletionPending} onClick={() => void deleteSourceProfile()}>
                  <Trash2 className="mr-1 h-4 w-4" />{profileDeletionPending ? "删除中…" : "删除"}
                </Button>
              </div>
              {sourceProfile && sourceProfileRuns.length > 0 && <p className="text-xs text-amber-700">这个 Profile 已被 {sourceProfileRuns.length} 个当前测评运行使用；请先删除相关运行。</p>}
            </Field>
            <Field label="实验名称"><Input value={profileName} onChange={(event) => { setProfileName(event.target.value); setProfileDirty(true); }} maxLength={255} /></Field>
          </div>

          <section className="space-y-3 rounded-lg border p-4">
            <div><h3 className="font-medium">文档与索引</h3><p className="text-xs text-muted-foreground">决定文档如何解析、切块和建立稀疏索引。</p></div>
            <div className="grid gap-2 sm:grid-cols-2">{ragCapabilities?.parsers.map((parser) => <Button key={parser.engine} type="button" variant={selectedParserEngine(draft) === parser.engine ? "default" : "outline"} disabled={!parser.available} onClick={() => changeIngestion("parserEngine", parser.engine)}>{parser.label || parser.engine}{!parser.available ? "（不可用）" : ""}</Button>) ?? <p className="text-xs text-muted-foreground">正在读取解析器能力…</p>}</div>
            {ragCapabilities?.parsers.filter((parser) => !parser.available).map((parser) => <p key={parser.engine} className="text-xs text-amber-700">{parser.label} 暂不可用：{parserReason(parser.reason)}</p>)}
            <div className="grid gap-2 sm:grid-cols-3">{sparseAnalyzerOptions.map((option) => <Button key={option.value} type="button" className="h-auto min-h-16 flex-col items-start whitespace-normal text-left" variant={selectedSparseAnalyzer(draft) === option.value ? "default" : "outline"} onClick={() => changeIngestion("sparseAnalyzer", option.value)}><span>{option.label}</span><span className="text-xs opacity-75">{option.description}</span></Button>)}</div>
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <NumberField label="切块大小" value={numberValue(draft?.ingestion?.chunkSize, 512)} min={128} max={8192} onChange={(value) => changeIngestion("chunkSize", value)} />
              <NumberField label="切块重叠" value={numberValue(draft?.ingestion?.chunkOverlap, 64)} min={0} max={Math.max(0, numberValue(draft?.ingestion?.chunkSize, 512) - 1)} onChange={(value) => changeIngestion("chunkOverlap", value)} />
            </div>
          </section>

          <section className="space-y-3 rounded-lg border p-4">
            <div><h3 className="font-medium">检索与回答</h3><p className="text-xs text-muted-foreground">这些参数可以在测评通过后单独发布为 Runtime 策略。</p></div>
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <NumberField label="最终返回条数" value={numberValue(draft?.runtime?.topN, 5)} min={1} max={100} onChange={(value) => changeRuntime("topN", value)} />
              <NumberField label="候选召回数" value={numberValue(draft?.runtime?.candidateTopK, 20)} min={numberValue(draft?.runtime?.topN, 5)} max={500} onChange={(value) => changeRuntime("candidateTopK", value)} />
              <NumberField label="最低相关分" value={numberValue(draft?.runtime?.minScore, 0.2)} min={0} max={1} step={0.05} onChange={(value) => changeRuntime("minScore", value)} />
              <NumberField label="回答温度" value={numberValue(draft?.runtime?.temperature, 0.1)} min={0} max={2} step={0.1} onChange={(value) => changeRuntime("temperature", value)} />
              <NumberField label="最大回答 Tokens" value={numberValue(draft?.runtime?.maxTokens, 1024)} min={1} max={131072} onChange={(value) => changeRuntime("maxTokens", value)} />
              <Field label="提示词版本"><Input value={String(draft?.runtime?.ragPromptBundleVersion ?? "rag-answer-v1")} onChange={(event) => changeRuntime("ragPromptBundleVersion", event.target.value)} /></Field>
              <Field label="回答模型（仅实验）"><Input value={String(draft?.answerModel ?? "")} onChange={(event) => changeProfile((profile) => { profile.answerModel = event.target.value; })} /></Field>
            </div>
            <div className="flex flex-wrap gap-4 text-sm">
              <Toggle label="Query Rewrite" checked={!!draft?.rewriteEnabled} onChange={(checked) => changeProfile((profile) => { profile.rewriteEnabled = checked; })} />
              <Toggle label="HyDE" checked={!!draft?.hydeEnabled} onChange={(checked) => changeProfile((profile) => { profile.hydeEnabled = checked; })} />
              <Toggle label="Reranker" checked={!!draft?.rerankerEnabled} onChange={(checked) => changeProfile((profile) => { profile.rerankerEnabled = checked; })} />
            </div>
            <p className="text-xs text-muted-foreground">Query Rewrite、HyDE、Reranker 和回答模型用于实验对比；当前发布门禁不会改动这些生产环境开关。</p>
          </section>

          <details className="rounded-lg border p-4">
            <summary className="cursor-pointer text-sm font-medium">高级：查看或编辑完整 JSON</summary>
            <p className="mt-2 text-xs text-muted-foreground">仅用于表单未覆盖的实验参数。普通实验无需展开。</p>
            <Textarea className="mt-3 min-h-80 font-mono text-xs" value={profileJSON} onChange={(event) => { setProfileJSON(event.target.value); setProfileDirty(true); setProfileMessage(""); }} />
          </details>
          {profileError && <p className="text-sm text-destructive">{profileError}</p>}
          {profileMessage && <p className="text-sm text-emerald-600">{profileMessage}</p>}
          <Button type="submit" disabled={saving || !profileName.trim() || !draft || !selectedParserEngine(draft)}><Save className="mr-2 h-4 w-4" />{saving ? "保存中…" : "保存 Profile"}</Button>
        </form>
      </CardContent>
    </Card>}

    {section !== "profile" && <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2"><ShieldCheck className="h-5 w-5" />把测评结果发布到正式 RAG</CardTitle>
        <CardDescription>这是上线操作，不是运行测评。先选发布类型，页面只会展示与它相关的步骤。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="grid gap-3 md:grid-cols-2">
          <Button type="button" className="h-auto items-start justify-start whitespace-normal p-4 text-left" variant={publishKind === "runtime" ? "default" : "outline"} onClick={() => choosePublishKind("runtime")}>
            <Gauge className="mr-3 mt-0.5 h-5 w-5 shrink-0" /><span><span className="block font-medium">Runtime · 在线参数</span><span className="mt-1 block text-xs opacity-80">影响新的检索/回答；多实例最多约 5 秒同步，不重建索引。</span></span>
          </Button>
          <Button type="button" className="h-auto items-start justify-start whitespace-normal p-4 text-left" variant={publishKind === "ingestion" ? "default" : "outline"} onClick={() => choosePublishKind("ingestion")}>
            <Database className="mr-3 mt-0.5 h-5 w-5 shrink-0" /><span><span className="block font-medium">Ingestion · 建库参数</span><span className="mt-1 block text-xs opacity-80">设为新知识库默认；已有知识库不会自动重建。</span></span>
          </Button>
        </div>

        <div className="rounded-lg bg-muted p-4 text-sm">
          {publishKind === "runtime" ? <p><strong>适合：</strong>topN、候选数、分数阈值、温度、回答长度、提示词版本。发布后新请求会读取新策略，现有索引不变。</p> : <p><strong>适合：</strong>解析器、切块、Sparse 分词、Embedding、富化参数。发布后只影响新建知识库；已有知识库需在知识库页面主动同步策略，后台建好新 generation 后才切换。</p>}
        </div>
        <p className="text-xs text-muted-foreground">候选运行用于提出变更，独立复验用于排除偶然波动；发布时后台还会检查配置的质量、样例数、错误率、延迟和成本门槛，未达标不会上线。</p>

        <div className="grid gap-4 lg:grid-cols-2">
          <Field label="1. 选择已成功完成的候选运行">
            <select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={runId} onChange={(event) => { setRunId(event.target.value); setConfirmationRunId(""); setPolicyError(""); }}>
              <option value="">请选择</option>
              {candidateRuns.map((run) => <option key={run.id} value={run.id}>{run.id} · {runModeLabel(run.mode)}</option>)}
            </select>
          </Field>
          <Field label="2. 选择独立复验运行">
            <select className="h-9 w-full rounded-md border bg-background px-3 text-sm" value={confirmationRunId} disabled={!selectedRun} onChange={(event) => setConfirmationRunId(event.target.value)}>
              <option value="">请选择</option>
              {confirmationRuns.map((run) => <option key={run.id} value={run.id}>{run.id} · {runModeLabel(run.mode)}</option>)}
            </select>
          </Field>
        </div>
        {selectedRun && confirmationRuns.length === 0 && <p className="text-xs text-amber-700">{publishKind === "runtime" ? "还缺少兼容复验：使用同一 Profile、同一数据集和同一 generation 再运行一次“仅在线模式”。" : "还缺少兼容复验：使用同一 Profile 和同一数据集再运行一次“完整 Pipeline”。"}</p>}
        {selectedProfile && <div className="rounded-md border px-3 py-2 text-sm"><span className="text-muted-foreground">随候选运行锁定的 Profile：</span>{profileOptionLabel(selectedProfile, profiles)}</div>}

        {publishKind === "runtime" && <div><Label>3. 选择要上线的参数</Label><div className="mt-2 flex flex-wrap gap-2">{runtimeWhitelist.map((field) => <Button key={field} type="button" size="sm" variant={fields.includes(field) ? "default" : "outline"} onClick={() => setFields((value) => value.includes(field) ? value.filter((item) => item !== field) : [...value, field])}>{runtimeFieldLabels[field]}</Button>)}</div><p className="mt-2 text-xs text-muted-foreground">未选字段继续使用当前线上值；索引和解析参数不会混入 Runtime 发布。</p></div>}

        <Field label={`${publishKind === "runtime" ? "4" : "3"}. 填写审计备注（必填）`}><Textarea value={note} onChange={(event) => setNote(event.target.value)} placeholder="例如：Open RAGBench v4 复验通过，提升 faithfulness 且延迟未退化" /></Field>
        {reasons.length > 0 && <div className="rounded bg-amber-500/10 p-3 text-xs text-amber-700">{reasons.map((reason) => <div key={reason}>• {reason}</div>)}</div>}
        {policyError && <p className="text-sm text-destructive">{policyError}</p>}
        {policyMessage && <p className="text-sm text-emerald-600">{policyMessage}</p>}
        <Button type="button" disabled={reasons.length > 0} onClick={() => void publish()}>{publishKind === "runtime" ? "发布 Runtime（立即生效）" : "发布 Ingestion（新建库默认）"}</Button>
      </CardContent>
    </Card>}

    {section !== "profile" && <Card>
      <CardHeader><CardTitle>发布与回滚记录</CardTitle><CardDescription>当前 Runtime v{runtime?.Version ?? "—"} · Ingestion v{ingestion?.Version ?? "—"}。Runtime 可在这里全局回滚；Ingestion 不会自动改动已有知识库。</CardDescription></CardHeader>
      <CardContent className="space-y-2">{audits.length === 0 ? <p className="text-sm text-muted-foreground">暂无审计记录</p> : audits.map((audit) => <div key={audit.ID} className="flex items-center justify-between gap-3 rounded border p-3 text-sm"><div><Badge variant="outline">{audit.PolicyKind === "ingestion" ? "INGESTION" : "RUNTIME"}</Badge><Badge variant="outline" className="ml-2">{audit.Action}</Badge><span className="ml-2">v{audit.FromVersion} → v{audit.ToVersion}</span><p className="mt-1 text-xs text-muted-foreground">{audit.Note || "无备注"} · {audit.ActorID}</p></div>{audit.PolicyKind === "runtime" && audit.Action === "PUBLISH" && audit.FromVersion > 0 && runtime && <Button size="sm" variant="ghost" onClick={() => void rollback(audit.FromVersion)}><RotateCcw className="mr-1 h-4 w-4" />回滚</Button>}</div>)}</CardContent>
    </Card>}
  </div>;
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <div className="space-y-2"><Label>{label}</Label>{children}</div>;
}

function NumberField({ label, value, min, max, step = 1, onChange }: {
  label: string;
  value: number;
  min: number;
  max: number;
  step?: number;
  onChange: (value: number) => void;
}) {
  return <Field label={label}><Input type="number" value={value} min={min} max={max} step={step} onChange={(event) => { const next = Number(event.target.value); if (Number.isFinite(next)) onChange(next); }} /></Field>;
}

function Toggle({ label, checked, onChange }: { label: string; checked: boolean; onChange: (checked: boolean) => void }) {
  return <label className="flex items-center gap-2"><input type="checkbox" checked={checked} onChange={(event) => onChange(event.target.checked)} />{label}</label>;
}
