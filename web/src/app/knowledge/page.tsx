"use client";

import * as React from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import {
  AlertCircle,
  CheckCircle2,
  ChevronRight,
  Database,
  FileText,
  Files,
  Loader2,
  MessageSquareText,
  Pencil,
  Plus,
  RefreshCw,
  Search,
  Sparkles,
  Trash2,
  Upload,
  XCircle,
} from "lucide-react";

import {
  createKnowledgeBase,
  deleteKnowledgeBase,
  deleteKnowledgeDocument,
  generateKnowledgeBaseMetadata,
  getRAGCapabilities,
  getMe,
  listKnowledgeBases,
  listKnowledgeDocuments,
  reindexKnowledgeDocument,
  searchKnowledgeBase,
  updateKnowledgeBase,
  uploadKnowledgeDocument,
  type KnowledgeBase,
  type KnowledgeDocument,
  type KnowledgeSearchHit,
  type RAGCapabilities,
  type RAGParseMode,
} from "@/lib/api";
import { RAGResourceGallery } from "@/components/rag-resource-gallery";
import {
  appendActAs,
  availableUploadExtensions,
  buildKnowledgeBasePayload,
  canChangeFeature,
  collectRAGResources,
  createPollController,
  documentActionLabel,
  documentNeedsLabel,
  formatDocumentAIBudget,
  formatParseModeTransition,
  getDocumentProgressLabel,
  getDocumentStatusState,
  getRAGCapabilityRows,
  getRAGOptInDisclosure,
  isAutoAvailable,
  isTerminalDocument,
  pdfAutoBehavior,
  shouldApplyDocumentLoad,
  shouldPollDocuments,
  uploadLimitForFile,
} from "@/components/rag-resource-gallery-state";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";

const DEFAULT_CHUNK_SIZE = 512;
const DEFAULT_CHUNK_OVERLAP = 64;

type KBForm = {
  name: string;
  description: string;
  chunkSize: string;
  chunkOverlap: string;
  parseMode: RAGParseMode;
  enrichmentEnabled: boolean;
};

const EMPTY_FORM: KBForm = {
  name: "",
  description: "",
  chunkSize: String(DEFAULT_CHUNK_SIZE),
  chunkOverlap: String(DEFAULT_CHUNK_OVERLAP),
  parseMode: "standard",
  enrichmentEnabled: false,
};

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

function formatTime(value?: string): string {
  if (!value) return "-";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "-" : date.toLocaleString("zh-CN");
}

function documentStatus(doc: KnowledgeDocument) {
  const state = getDocumentStatusState(doc);
  if (state.kind === "failed") {
    return {
      ...state,
      icon: XCircle,
      className: "border-destructive/25 bg-destructive/10 text-destructive",
    };
  }
  if (state.kind === "degraded") {
    return {
      ...state,
      icon: CheckCircle2,
      className: "border-amber-500/25 bg-amber-500/10 text-amber-800 dark:text-amber-300",
    };
  }
  if (state.kind === "done") {
    return {
      ...state,
      icon: CheckCircle2,
      className: "border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
    };
  }
  return {
    ...state,
    icon: Loader2,
    className: state.kind === "queued"
      ? "border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-400"
      : "border-blue-500/25 bg-blue-500/10 text-blue-700 dark:text-blue-400",
  };
}

function progressText(doc: KnowledgeDocument): string {
  const label = getDocumentProgressLabel(doc);
  if (doc.progress.total <= 0) return label;
  const units: Record<string, string> = {
    pages: "页",
    assets: "张图片",
    blocks: "个区块",
    chunks: "个分片",
  };
  const unit = units[doc.progress.unit.toLowerCase()] || doc.progress.unit;
  return `${label} ${doc.progress.current}/${doc.progress.total}${unit ? ` ${unit}` : ""}`;
}

export default function KnowledgePage() {
  const searchParams = useSearchParams();
  const actAs = searchParams.get("actAs")?.trim() || "";
  const [knowledgeBases, setKnowledgeBases] = React.useState<KnowledgeBase[]>([]);
  const [capabilities, setCapabilities] = React.useState<RAGCapabilities | null>(null);
  const [selectedId, setSelectedId] = React.useState("");
  const selectedIdRef = React.useRef("");
  const documentsRequestGenerationRef = React.useRef(0);
  const [documents, setDocuments] = React.useState<KnowledgeDocument[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [documentsLoading, setDocumentsLoading] = React.useState(false);
  const [readOnly, setReadOnly] = React.useState(false);
  const [error, setError] = React.useState("");
  const [notice, setNotice] = React.useState("");

  const [formOpen, setFormOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<KnowledgeBase | null>(null);
  const [form, setForm] = React.useState<KBForm>(EMPTY_FORM);
  const [formError, setFormError] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [pendingOptIn, setPendingOptIn] = React.useState<"auto" | "enrichment" | null>(null);
  const [generatingMetadata, setGeneratingMetadata] = React.useState(false);
  const metadataRequestRef = React.useRef<AbortController | null>(null);
  const [deleteKBTarget, setDeleteKBTarget] = React.useState<KnowledgeBase | null>(null);
  const [deleteDocTarget, setDeleteDocTarget] = React.useState<KnowledgeDocument | null>(null);

  const [uploading, setUploading] = React.useState(false);
  const [uploadProgress, setUploadProgress] = React.useState("");
  const [dragging, setDragging] = React.useState(false);
  const fileInputRef = React.useRef<HTMLInputElement>(null);

  const [query, setQuery] = React.useState("");
  const [topN, setTopN] = React.useState(5);
  const [searching, setSearching] = React.useState(false);
  const [searched, setSearched] = React.useState(false);
  const [hits, setHits] = React.useState<KnowledgeSearchHit[]>([]);

  const selected = React.useMemo(
    () => knowledgeBases.find((kb) => kb.id === selectedId) ?? null,
    [knowledgeBases, selectedId],
  );
  const uploadExtensions = React.useMemo(
    () => availableUploadExtensions(capabilities),
    [capabilities],
  );
  const uploadLimits = React.useMemo(() => {
    const groups = new Map<number, string[]>();
    for (const extension of uploadExtensions) {
      const limit = uploadLimitForFile(`file${extension}`, capabilities);
      if (limit <= 0) continue;
      const extensions = groups.get(limit) || [];
      extensions.push(extension.slice(1).toUpperCase());
      groups.set(limit, extensions);
    }
    return [...groups.entries()]
      .map(([limit, extensions]) => `${extensions.join("/")} ${formatBytes(limit)}`)
      .join("；");
  }, [capabilities, uploadExtensions]);
  const autoAvailable = isAutoAvailable(capabilities);

  const selectKnowledgeBase = React.useCallback((nextID: string) => {
    if (selectedIdRef.current !== nextID) {
      selectedIdRef.current = nextID;
      documentsRequestGenerationRef.current += 1;
    }
    setSelectedId(nextID);
  }, []);

  const loadKnowledgeBases = React.useCallback(async (preferredId?: string) => {
    setLoading(true);
    setError("");
    try {
      const rows = await listKnowledgeBases();
      setKnowledgeBases(rows);
      const desired = preferredId || selectedIdRef.current;
      selectKnowledgeBase(rows.some((kb) => kb.id === desired) ? desired : rows[0]?.id || "");
    } catch (err) {
      setKnowledgeBases([]);
      selectKnowledgeBase("");
      setError(errorMessage(err, "读取知识库失败"));
    } finally {
      setLoading(false);
    }
  }, [selectKnowledgeBase]);

  const loadDocuments = React.useCallback(async (
    kbId: string,
    quiet = false,
    signal?: AbortSignal,
  ) => {
    if (!kbId || selectedIdRef.current !== kbId || signal?.aborted) return;
    const requestGeneration = documentsRequestGenerationRef.current + 1;
    documentsRequestGenerationRef.current = requestGeneration;
    if (!quiet) setDocumentsLoading(true);
    try {
      const rows = await listKnowledgeDocuments(kbId, signal);
      if (!shouldApplyDocumentLoad(
        kbId,
        requestGeneration,
        selectedIdRef.current,
        documentsRequestGenerationRef.current,
        signal?.aborted === true,
      )) return;
      setDocuments(rows);
    } catch (err) {
      if (shouldApplyDocumentLoad(
        kbId,
        requestGeneration,
        selectedIdRef.current,
        documentsRequestGenerationRef.current,
        signal?.aborted === true,
      ) && !quiet) setError(errorMessage(err, "读取文档失败"));
    } finally {
      if (shouldApplyDocumentLoad(
        kbId,
        requestGeneration,
        selectedIdRef.current,
        documentsRequestGenerationRef.current,
        signal?.aborted === true,
      )) {
        setDocumentsLoading(false);
      }
    }
  }, []);

  React.useEffect(() => {
    void loadKnowledgeBases();
    void getRAGCapabilities()
      .then(setCapabilities)
      .catch((err) => {
        setCapabilities(null);
        setError(errorMessage(err, "读取 RAG 能力失败，上传与高级配置已停用"));
      });
    void getMe().then((me) => setReadOnly(me.readOnly === true)).catch(() => undefined);
  }, [actAs, loadKnowledgeBases]);

  React.useEffect(() => {
    setDocuments([]);
    setHits([]);
    setSearched(false);
    if (!selectedId) return;
    const controller = new AbortController();
    void loadDocuments(selectedId, false, controller.signal);
    return () => controller.abort();
  }, [selectedId, loadDocuments]);

  const indexing = shouldPollDocuments(documents);
  const readyDocumentCount = documents.filter((doc) =>
    doc.status.toUpperCase() === "DONE",
  ).length;

  React.useEffect(() => {
    if (!selectedId || !indexing) return;
    const controller = new AbortController();
    const poller = createPollController(
      (callback, delay) => window.setTimeout(callback, delay),
      (handle) => window.clearTimeout(handle as number),
    );
    const poll = () => {
      void loadDocuments(selectedId, true, controller.signal).finally(() => {
        if (!controller.signal.aborted) poller.schedule(poll, 2500);
      });
    };
    poller.schedule(poll, 2500);
    return () => {
      controller.abort();
      poller.stop();
    };
  }, [selectedId, indexing, loadDocuments]);

  const openCreate = () => {
    metadataRequestRef.current?.abort();
    metadataRequestRef.current = null;
    setGeneratingMetadata(false);
    setEditing(null);
    setForm(EMPTY_FORM);
    setPendingOptIn(null);
    setFormError("");
    setFormOpen(true);
  };

  const openEdit = (kb: KnowledgeBase) => {
    metadataRequestRef.current?.abort();
    metadataRequestRef.current = null;
    setGeneratingMetadata(false);
    setEditing(kb);
    setForm({
      name: kb.name,
      description: kb.description,
      chunkSize: String(kb.chunkSize),
      chunkOverlap: String(kb.chunkOverlap),
      parseMode: kb.parseMode || "standard",
      enrichmentEnabled: kb.enrichmentEnabled === true,
    });
    setPendingOptIn(null);
    setFormError("");
    setFormOpen(true);
  };

  const generateMetadata = async (kb: KnowledgeBase) => {
    openEdit(kb);
    const controller = new AbortController();
    metadataRequestRef.current = controller;
    setGeneratingMetadata(true);
    setFormError("");
    try {
      const generated = await generateKnowledgeBaseMetadata(kb.id, controller.signal);
      if (metadataRequestRef.current !== controller || controller.signal.aborted) return;
      setForm((current) => ({
        ...current,
        name: generated.name,
        description: generated.description,
      }));
    } catch (err) {
      if (controller.signal.aborted) return;
      setFormError(errorMessage(err, "AI 生成名称和描述失败"));
    } finally {
      if (metadataRequestRef.current === controller) {
        metadataRequestRef.current = null;
        setGeneratingMetadata(false);
      }
    }
  };

  const setKnowledgeFormOpen = (open: boolean) => {
    if (!open) {
      metadataRequestRef.current?.abort();
      metadataRequestRef.current = null;
      setGeneratingMetadata(false);
      setPendingOptIn(null);
    }
    setFormOpen(open);
  };

  const saveKnowledgeBase = async (event: React.FormEvent) => {
    event.preventDefault();
    const name = form.name.trim();
    const chunkSize = Number(form.chunkSize);
    const chunkOverlap = Number(form.chunkOverlap);
    if (!name) {
      setFormError("请输入知识库名称");
      return;
    }
    if (!Number.isInteger(chunkSize) || chunkSize <= 0) {
      setFormError("分片长度必须是大于 0 的整数");
      return;
    }
    if (!Number.isInteger(chunkOverlap) || chunkOverlap < 0 || chunkOverlap >= chunkSize) {
      setFormError("重叠长度必须是小于分片长度的非负整数");
      return;
    }
    setSaving(true);
    setFormError("");
    try {
      const payload = buildKnowledgeBasePayload({
        name,
        description: form.description,
        chunkSize,
        chunkOverlap,
        parseMode: form.parseMode,
        enrichmentEnabled: form.enrichmentEnabled,
      });
      const wasEditing = !!editing;
      const saved = editing
        ? await updateKnowledgeBase(editing.id, payload)
        : await createKnowledgeBase(payload);
      setFormOpen(false);
      setNotice(editing ? "知识库设置已保存" : "知识库已创建，可以开始上传文档");
      window.setTimeout(() => setNotice(""), 2500);
      await loadKnowledgeBases(saved.id);
      if (wasEditing && saved.id === selectedId) {
        await loadDocuments(saved.id, true);
      }
    } catch (err) {
      setFormError(errorMessage(err, "保存知识库失败"));
    } finally {
      setSaving(false);
    }
  };

  const removeKnowledgeBase = async () => {
    if (!deleteKBTarget) return;
    setSaving(true);
    setError("");
    try {
      await deleteKnowledgeBase(deleteKBTarget.id);
      setDeleteKBTarget(null);
      await loadKnowledgeBases();
    } catch (err) {
      setError(errorMessage(err, "删除知识库失败"));
    } finally {
      setSaving(false);
    }
  };

  const uploadFiles = async (files: File[]) => {
    if (!selected || files.length === 0) return;
    const invalid = files.find((file) => uploadLimitForFile(file.name, capabilities) <= 0);
    if (invalid) {
      setError(`${invalid.name} 的格式当前不可用；可上传 ${uploadExtensions.map((extension) => extension.slice(1).toUpperCase()).join("、") || "能力接口允许的格式"}`);
      return;
    }
    const tooLarge = files.find((file) => file.size > uploadLimitForFile(file.name, capabilities));
    if (tooLarge) {
      setError(`${tooLarge.name} 超过该格式 ${formatBytes(uploadLimitForFile(tooLarge.name, capabilities))} 的大小限制`);
      return;
    }
    setUploading(true);
    setError("");
    try {
      for (let index = 0; index < files.length; index += 1) {
        setUploadProgress(`正在上传 ${index + 1}/${files.length}：${files[index].name}`);
        await uploadKnowledgeDocument(selected.id, files[index]);
      }
      setUploadProgress("");
      await loadDocuments(selected.id);
    } catch (err) {
      setError(errorMessage(err, "上传文档失败"));
    } finally {
      setUploading(false);
      setUploadProgress("");
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  };

  const removeDocument = async () => {
    if (!selected || !deleteDocTarget) return;
    setSaving(true);
    try {
      await deleteKnowledgeDocument(selected.id, deleteDocTarget.id);
      setDeleteDocTarget(null);
      await loadDocuments(selected.id);
    } catch (err) {
      setError(errorMessage(err, "删除文档失败"));
    } finally {
      setSaving(false);
    }
  };

  const reindexDocument = async (doc: KnowledgeDocument) => {
    if (!selected) return;
    setError("");
    try {
      await reindexKnowledgeDocument(selected.id, doc.id);
      await loadDocuments(selected.id);
    } catch (err) {
      setError(errorMessage(err, "重新索引失败"));
    }
  };

  const runSearch = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!selected || !query.trim()) return;
    setSearching(true);
    setError("");
    setSearched(true);
    try {
      setHits(await searchKnowledgeBase(selected.id, query.trim(), topN));
    } catch (err) {
      setHits([]);
      setError(errorMessage(err, "检索失败"));
    } finally {
      setSearching(false);
    }
  };

  const changeAutoMode = (enabled: boolean) => {
    const currentlyEnabled = form.parseMode === "auto";
    if (!canChangeFeature(currentlyEnabled, enabled, autoAvailable)) {
      setFormError(capabilities?.advanced.reason || "高级 RAG 当前不可用，无法新开启");
      return;
    }
    setFormError("");
    if (enabled && !currentlyEnabled) {
      setPendingOptIn("auto");
      return;
    }
    setForm((current) => ({ ...current, parseMode: enabled ? "auto" : "standard" }));
  };

  const changeEnrichment = (enabled: boolean) => {
    const currentlyEnabled = form.enrichmentEnabled;
    const available = capabilities?.enrichment.available === true;
    if (!canChangeFeature(currentlyEnabled, enabled, available)) {
      setFormError(capabilities?.enrichment.reason || "表格/代码语义增强当前不可用，无法新开启");
      return;
    }
    setFormError("");
    if (enabled && !currentlyEnabled) {
      setPendingOptIn("enrichment");
      return;
    }
    setForm((current) => ({ ...current, enrichmentEnabled: enabled }));
  };

  const confirmOptIn = () => {
    if (pendingOptIn === "auto") {
      setForm((current) => ({ ...current, parseMode: "auto" }));
    } else if (pendingOptIn === "enrichment") {
      setForm((current) => ({ ...current, enrichmentEnabled: true }));
    }
    setPendingOptIn(null);
  };

  const capabilityRows = getRAGCapabilityRows(capabilities);

  return (
    <div className="mx-auto max-w-7xl space-y-6 p-4 sm:p-6">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">知识库</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            上传业务资料并完成向量索引，再授权给需要使用它的智能体。
          </p>
        </div>
        <Button onClick={openCreate} disabled={readOnly}>
          <Plus className="size-4" />
          新建知识库
        </Button>
      </div>

      {readOnly && (
        <Message tone="warning">
          当前正在只读查看其他用户，不能创建、上传或修改知识库。
        </Message>
      )}
      {error && <Message tone="error">{error}</Message>}
      {notice && <Message tone="success">{notice}</Message>}

      {loading ? (
        <div className="grid gap-4 lg:grid-cols-[300px_minmax(0,1fr)]">
          <Skeleton className="h-[520px] rounded-xl" />
          <Skeleton className="h-[520px] rounded-xl" />
        </div>
      ) : knowledgeBases.length === 0 && !error ? (
        <Card className="min-h-[420px] items-center justify-center text-center">
          <CardContent className="max-w-md space-y-4">
            <div className="mx-auto flex size-14 items-center justify-center rounded-2xl bg-primary/10">
              <Database className="size-7 text-primary" />
            </div>
            <div>
              <h2 className="font-medium">还没有知识库</h2>
              <p className="mt-1 text-sm text-muted-foreground">
                新建知识库并上传能力接口允许的文档后，即可让智能体检索。
              </p>
            </div>
            <Button onClick={openCreate} disabled={readOnly}>
              <Plus className="size-4" />
              新建知识库
            </Button>
          </CardContent>
        </Card>
      ) : (
        <div className="grid items-start gap-4 lg:grid-cols-[300px_minmax(0,1fr)]">
          <Card className="gap-2 lg:sticky lg:top-6">
            <CardHeader className="border-b">
              <CardTitle>我的知识库</CardTitle>
              <CardDescription>{knowledgeBases.length} 个知识库</CardDescription>
            </CardHeader>
            <CardContent className="space-y-2 px-2">
              {knowledgeBases.map((kb) => (
                <button
                  key={kb.id}
                  type="button"
                  onClick={() => selectKnowledgeBase(kb.id)}
                  className={cn(
                    "flex w-full items-center gap-3 rounded-lg px-3 py-3 text-left transition-colors",
                    selectedId === kb.id ? "bg-primary/10 text-foreground" : "hover:bg-muted",
                  )}
                >
                  <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-background ring-1 ring-foreground/10">
                    <Database className="size-4 text-primary" />
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-sm font-medium">{kb.name}</span>
                    <span className="mt-0.5 block truncate text-xs text-muted-foreground">
                      {kb.description || `${kb.embedDims} 维向量`}
                    </span>
                  </span>
                  <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
                </button>
              ))}
            </CardContent>
          </Card>

          {selected && (
            <div className="min-w-0 space-y-4">
              <Card>
                <CardHeader className="border-b">
                  <CardTitle className="flex items-center gap-2">
                    {selected.name}
                    <Badge variant="outline" className="border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400">
                      {selected.status === "active" ? "可用" : selected.status}
                    </Badge>
                  </CardTitle>
                  <CardDescription>
                    {selected.description || "未填写描述"}
                  </CardDescription>
                  <CardAction className="flex gap-1">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => void generateMetadata(selected)}
                      disabled={readOnly || documentsLoading || generatingMetadata || readyDocumentCount === 0}
                      title={readyDocumentCount === 0 ? "请先上传并等待文档处理完成" : "根据已完成处理的文档生成名称和描述"}
                    >
                      {generatingMetadata ? <Loader2 className="size-4 animate-spin" /> : <Sparkles className="size-4" />}
                      {generatingMetadata ? "生成中" : "AI 生成"}
                    </Button>
                    <Button variant="ghost" size="icon" onClick={() => openEdit(selected)} disabled={readOnly} aria-label="编辑知识库">
                      <Pencil className="size-4" />
                    </Button>
                    <Button variant="ghost" size="icon" onClick={() => setDeleteKBTarget(selected)} disabled={readOnly} aria-label="删除知识库" className="text-muted-foreground hover:text-destructive">
                      <Trash2 className="size-4" />
                    </Button>
                  </CardAction>
                </CardHeader>
                <CardContent className="grid gap-4 sm:grid-cols-2 xl:grid-cols-6">
                  <Metadata label="Embedding 模型" value={selected.embedModel} />
                  <Metadata label="向量维度" value={`${selected.embedDims} 维`} />
                  <Metadata label="分片长度" value={`${selected.chunkSize} tokens`} />
                  <Metadata label="重叠长度" value={`${selected.chunkOverlap} tokens`} />
                  <Metadata label="解析模式" value={selected.parseMode === "auto" ? "高级 RAG" : "标准解析"} />
                  <Metadata label="表格/代码增强" value={selected.enrichmentEnabled ? "已开启" : "已关闭"} />
                </CardContent>
              </Card>

              <Card>
                <CardHeader className="border-b">
                  <CardTitle>文档</CardTitle>
                  <CardDescription>上传后由服务器异步解析、切片并写入 Milvus。</CardDescription>
                  <CardAction>
                    <input
                      ref={fileInputRef}
                      type="file"
                      className="hidden"
                      accept={uploadExtensions.join(",")}
                      multiple
                      onChange={(event) => void uploadFiles(Array.from(event.target.files || []))}
                    />
                    <Button variant="outline" size="sm" onClick={() => fileInputRef.current?.click()} disabled={readOnly || uploading || uploadExtensions.length === 0}>
                      {uploading ? <Loader2 className="size-4 animate-spin" /> : <Upload className="size-4" />}
                      {uploading ? "上传中" : "上传文档"}
                    </Button>
                  </CardAction>
                </CardHeader>
                <CardContent className="space-y-4">
                  {selected.parseMode === "auto" && capabilities && pdfAutoBehavior(selected.parseMode, capabilities) === "native-fallback" && (
                    <Message tone="warning">
                      PDF 视觉解析当前不可用；上传到此高级 RAG 知识库的 PDF 会使用原生文字解析并记录降级。{capabilities.pdfAuto.reason ? `原因：${capabilities.pdfAuto.reason}` : ""}
                    </Message>
                  )}
                  <button
                    type="button"
                    disabled={readOnly || uploading || uploadExtensions.length === 0}
                    onClick={() => fileInputRef.current?.click()}
                    onDragEnter={(event) => { event.preventDefault(); setDragging(true); }}
                    onDragOver={(event) => event.preventDefault()}
                    onDragLeave={() => setDragging(false)}
                    onDrop={(event) => {
                      event.preventDefault();
                      setDragging(false);
                      void uploadFiles(Array.from(event.dataTransfer.files));
                    }}
                    className={cn(
                      "flex w-full flex-col items-center justify-center rounded-xl border border-dashed px-4 py-7 text-center transition-colors",
                      dragging ? "border-primary bg-primary/5" : "border-border hover:bg-muted/40",
                      (readOnly || uploading || uploadExtensions.length === 0) && "cursor-not-allowed opacity-60",
                    )}
                  >
                    {uploading ? <Loader2 className="mb-2 size-6 animate-spin text-primary" /> : <Upload className="mb-2 size-6 text-muted-foreground" />}
                    <span className="text-sm font-medium">{uploadProgress || "拖放文件到这里，或点击选择"}</span>
                    <span className="mt-1 text-xs text-muted-foreground">
                      {uploadExtensions.length > 0
                        ? `支持 ${uploadExtensions.map((extension) => extension.slice(1).toUpperCase()).join("、")}；${uploadLimits || "大小限制由服务器决定"}`
                        : "当前没有可上传的文档格式"}
                    </span>
                  </button>

                  {documentsLoading ? (
                    <div className="space-y-2">
                      {[1, 2].map((item) => <Skeleton key={item} className="h-14 w-full" />)}
                    </div>
                  ) : documents.length === 0 ? (
                    <div className="flex flex-col items-center py-8 text-center text-muted-foreground">
                      <Files className="mb-2 size-8 opacity-50" />
                      <p className="text-sm">尚未上传文档</p>
                    </div>
                  ) : (
                    <div className="overflow-x-auto rounded-lg border">
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead>文件</TableHead>
                            <TableHead>状态</TableHead>
                            <TableHead>索引</TableHead>
                            <TableHead>上传时间</TableHead>
                            <TableHead className="text-right">操作</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {documents.map((doc) => {
                            const status = documentStatus(doc);
                            const StatusIcon = status.icon;
                            const needsLabel = documentNeedsLabel(doc);
                            return (
                              <TableRow key={doc.id}>
                                <TableCell>
                                  <div className="flex min-w-[190px] items-center gap-3">
                                    <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-muted"><FileText className="size-4" /></span>
                                    <span className="min-w-0">
                                      <span className="block max-w-[260px] truncate text-sm font-medium" title={doc.fileName}>{doc.fileName}</span>
                                      <span className="text-xs text-muted-foreground">{formatBytes(doc.fileSize)} · v{doc.version}</span>
                                      <span className="block text-[11px] text-muted-foreground">
                                        {formatParseModeTransition(doc)}
                                      </span>
                                    </span>
                                  </div>
                                  {doc.errorMsg && <p className="mt-1 max-w-[320px] text-xs text-destructive">{doc.errorMsg}</p>}
                                </TableCell>
                                <TableCell>
                                  <Badge variant="outline" className={status.className}>
                                    <StatusIcon className={cn("size-3", status.spinning && "animate-spin")} />
                                    {status.label}
                                  </Badge>
                                  <p className="mt-1 max-w-48 text-xs text-muted-foreground">{progressText(doc)}</p>
                                  {doc.warningCount > 0 && (
                                    <p className="mt-0.5 text-xs text-amber-700 dark:text-amber-300">{doc.warningCount} 条警告</p>
                                  )}
                                </TableCell>
                                <TableCell className="whitespace-nowrap text-sm text-muted-foreground">
                                  {doc.status.toUpperCase() === "DONE" ? `${doc.chunkCount} 段 · ${doc.tokenCount} tokens` : "-"}
                                  {needsLabel && (
                                    <Badge variant="outline" className="ml-2 border-amber-500/25 bg-amber-500/10 text-amber-800 dark:text-amber-300">
                                      {needsLabel}
                                    </Badge>
                                  )}
                                </TableCell>
                                <TableCell className="whitespace-nowrap text-sm text-muted-foreground">{formatTime(doc.uploadedAt)}</TableCell>
                                <TableCell>
                                  <div className="flex justify-end gap-1">
                                    <Button
                                      variant={needsLabel ? "outline" : "ghost"}
                                      size={needsLabel ? "sm" : "icon"}
                                      aria-label={documentActionLabel(doc)}
                                      title={documentActionLabel(doc)}
                                      onClick={() => void reindexDocument(doc)}
                                      disabled={readOnly || !isTerminalDocument(doc)}
                                    >
                                      <RefreshCw className="size-4" />
                                      {needsLabel && documentActionLabel(doc)}
                                    </Button>
                                    <Button variant="ghost" size="icon" aria-label="删除文档" title="删除文档" onClick={() => setDeleteDocTarget(doc)} disabled={readOnly} className="text-muted-foreground hover:text-destructive">
                                      <Trash2 className="size-4" />
                                    </Button>
                                  </div>
                                </TableCell>
                              </TableRow>
                            );
                          })}
                        </TableBody>
                      </Table>
                    </div>
                  )}
                </CardContent>
              </Card>

              <Card>
                <CardHeader className="border-b">
                  <CardTitle>检索测试</CardTitle>
                  <CardDescription>直接查询当前知识库，确认切片内容与召回效果。</CardDescription>
                  <CardAction>
                    <Link
                      href={appendActAs(`/knowledge/chat/?id=${encodeURIComponent(selected.id)}`, actAs)}
                      className={buttonVariants({ size: "sm" })}
                    >
                      <MessageSquareText className="size-3.5" />
                      开始问答
                    </Link>
                  </CardAction>
                </CardHeader>
                <CardContent className="space-y-4">
                  <form onSubmit={runSearch} className="flex flex-col gap-2 sm:flex-row">
                    <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="输入要检索的问题或关键词" className="flex-1" />
                    <div className="flex gap-2">
                      <div className="flex items-center gap-2 rounded-md border px-3">
                        <Label htmlFor="search-top-n" className="whitespace-nowrap text-xs text-muted-foreground">Top N</Label>
                        <Input id="search-top-n" type="number" min={1} max={20} value={topN} onChange={(event) => setTopN(Math.max(1, Math.min(20, Number(event.target.value) || 1)))} className="h-8 w-14 border-0 px-1 shadow-none focus-visible:ring-0" />
                      </div>
                      <Button type="submit" disabled={searching || !query.trim()}>
                        {searching ? <Loader2 className="size-4 animate-spin" /> : <Search className="size-4" />}
                        检索
                      </Button>
                    </div>
                  </form>

                  {searched && !searching && hits.length === 0 && (
                    <div className="rounded-lg border border-dashed py-8 text-center text-sm text-muted-foreground">没有检索到相关内容</div>
                  )}
                  {hits.length > 0 && (
                    <div className="space-y-3">
                      {hits.map((hit, index) => (
                        <div key={`${hit.docId}-${hit.chunkIndex}-${index}`} className="rounded-lg border bg-muted/20 p-4">
                          <div className="mb-2 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                            <Badge variant="secondary">#{index + 1}</Badge>
                            <span className="font-medium text-foreground">{hit.docName}</span>
                            {hit.sectionTitle && <span>· {hit.sectionTitle}</span>}
                            {hit.pageNum ? <span>· 第 {hit.pageNum} 页</span> : null}
                            <span>· 分片 {hit.chunkIndex + 1}</span>
                            <span className="ml-auto font-mono">score {hit.score.toFixed(4)}</span>
                          </div>
                          <p className="whitespace-pre-wrap text-sm leading-6">{hit.content}</p>
                          <RAGResourceGallery
                            resources={collectRAGResources([hit])}
                            actAs={actAs}
                            title="命中图片"
                            compact
                            className="mt-3"
                          />
                        </div>
                      ))}
                    </div>
                  )}
                </CardContent>
              </Card>
            </div>
          )}
        </div>
      )}

      <Dialog open={formOpen} onOpenChange={setKnowledgeFormOpen}>
        <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-2xl">
          <form onSubmit={saveKnowledgeBase}>
            <DialogHeader>
              <DialogTitle>{editing ? "编辑知识库" : "新建知识库"}</DialogTitle>
              <DialogDescription>
                {editing
                  ? "分片单位是 token。修改分片参数后，已有文档需要重新索引才会生效。"
                  : "可以先填写临时名称和描述。上传并等待文档处理完成后，可在知识库详情页使用 AI 自动生成名称和描述。创建后会绑定当前 embedding 模型与向量维度。"}
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4 py-5">
              {formError && <Message tone="error">{formError}</Message>}
              {generatingMetadata && (
                <div className="flex items-center gap-2 rounded-lg bg-primary/5 p-3 text-sm text-muted-foreground">
                  <Loader2 className="size-4 animate-spin text-primary" />
                  正在根据已完成处理的文档生成名称和描述…
                </div>
              )}
              <div className="space-y-2">
                <Label htmlFor="kb-name">名称</Label>
                <Input id="kb-name" autoFocus value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} placeholder="例如：产品与售后资料" maxLength={120} disabled={generatingMetadata} />
              </div>
              <div className="space-y-2">
                <Label htmlFor="kb-description">描述</Label>
                <Textarea id="kb-description" value={form.description} onChange={(event) => setForm((current) => ({ ...current, description: event.target.value }))} placeholder="说明这个知识库包含哪些内容，主要用途是什么（重要！！知识库的名称和描述将影响 Agent 使用知识库）" rows={3} disabled={generatingMetadata} />
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-2">
                  <Label htmlFor="kb-chunk-size">分片长度</Label>
                  <Input id="kb-chunk-size" type="number" min={1} value={form.chunkSize} onChange={(event) => setForm((current) => ({ ...current, chunkSize: event.target.value }))} />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="kb-chunk-overlap">重叠长度</Label>
                  <Input id="kb-chunk-overlap" type="number" min={0} value={form.chunkOverlap} onChange={(event) => setForm((current) => ({ ...current, chunkOverlap: event.target.value }))} />
                </div>
              </div>
              <div className="space-y-3 rounded-lg border p-4">
                <div className="flex items-start justify-between gap-4">
                  <div className="space-y-1">
                    <Label htmlFor="kb-auto-mode">高级 RAG</Label>
                    <p className="text-xs leading-5 text-muted-foreground">
                      开启后使用 <code>parseMode: auto</code>。复杂 PDF 页面和 Office 图片可能发送给管理员配置的第三方 DocumentAI。
                    </p>
                  </div>
                  <Switch
                    id="kb-auto-mode"
                    checked={form.parseMode === "auto"}
                    onCheckedChange={changeAutoMode}
                    disabled={saving || generatingMetadata || (!autoAvailable && form.parseMode !== "auto")}
                    aria-label="高级 RAG"
                  />
                </div>
                <p className="text-xs font-medium text-muted-foreground">{formatDocumentAIBudget(capabilities)}</p>
                {form.parseMode === "auto" && capabilities && !capabilities.pdfAuto.available && (
                  <Message tone="warning">
                    PDF auto 当前不可用，PDF 会走原生文字解析；这不会阻止 Office auto。{capabilities.pdfAuto.reason ? `原因：${capabilities.pdfAuto.reason}` : ""}
                  </Message>
                )}
              </div>

              <div className="space-y-3 rounded-lg border p-4">
                <div className="flex items-start justify-between gap-4">
                  <div className="space-y-1">
                    <Label htmlFor="kb-enrichment">表格/代码语义增强</Label>
                    <p className="text-xs leading-5 text-muted-foreground">
                      独立 opt-in；无论标准或高级模式，表格与代码原文都会发送给管理员配置的第三方 DocumentAI TextModel。
                    </p>
                  </div>
                  <Switch
                    id="kb-enrichment"
                    checked={form.enrichmentEnabled}
                    onCheckedChange={changeEnrichment}
                    disabled={saving || generatingMetadata || (capabilities?.enrichment.available !== true && !form.enrichmentEnabled)}
                    aria-label="表格和代码语义增强"
                  />
                </div>
                <p className="text-xs font-medium text-muted-foreground">{formatDocumentAIBudget(capabilities)}</p>
                {capabilities?.enrichment.available !== true && (
                  <p className="text-xs text-amber-700 dark:text-amber-300">
                    当前不能新开启：{capabilities?.enrichment.reason || "系统开关或 TextModel 不可用"}。已开启的知识库仍可在此关闭。
                  </p>
                )}
              </div>

              <div className="space-y-2 rounded-lg bg-muted/50 p-3">
                <p className="text-xs font-medium">系统能力</p>
                {capabilityRows.length === 0 ? (
                  <p className="text-xs text-muted-foreground">能力信息尚未加载，高级配置保持关闭。</p>
                ) : capabilityRows.map(({ label, capability }) => (
                  <div key={label} className="flex items-start justify-between gap-3 text-xs">
                    <span>{label}</span>
                    <span className={cn(
                      "max-w-[70%] text-right",
                      capability.available ? "text-emerald-700 dark:text-emerald-400" : "text-amber-700 dark:text-amber-300",
                    )}>
                      {capability.available ? "可用" : capability.reason || "不可用"}
                    </span>
                  </div>
                ))}
              </div>
              {editing && (
                <div className="rounded-lg bg-muted/60 p-3 text-xs text-muted-foreground">
                  已绑定 {editing.embedModel}（{editing.embedDims} 维）。修改解析模式后文档会标记为需要重新解析；修改增强或分片参数后会标记为需要重新索引，不会自动重跑。
                </div>
              )}
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setKnowledgeFormOpen(false)}>取消</Button>
              <Button type="submit" disabled={saving || generatingMetadata}>
                {saving && <Loader2 className="size-4 animate-spin" />}
                {saving ? "保存中" : "保存"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={pendingOptIn !== null} onOpenChange={(open) => !open && setPendingOptIn(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {pendingOptIn === "auto" ? "开启高级 RAG？" : "开启表格/代码语义增强？"}
            </AlertDialogTitle>
            <AlertDialogDescription className="space-y-2">
              <span className="block">
                {getRAGOptInDisclosure(pendingOptIn || "enrichment")}
              </span>
              <span className="block font-medium text-foreground">{formatDocumentAIBudget(capabilities)}</span>
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={confirmOptIn}>确认开启</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={!!deleteKBTarget} onOpenChange={(open) => !open && setDeleteKBTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除知识库“{deleteKBTarget?.name}”？</AlertDialogTitle>
            <AlertDialogDescription>文档原件、索引和向量数据都会被永久删除，智能体也将无法继续检索它。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => void removeKnowledgeBase()} disabled={saving} className="bg-destructive text-destructive-foreground hover:bg-destructive/90">删除</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={!!deleteDocTarget} onOpenChange={(open) => !open && setDeleteDocTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除文档“{deleteDocTarget?.fileName}”？</AlertDialogTitle>
            <AlertDialogDescription>文档原件及其全部向量分片都会被永久删除。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => void removeDocument()} disabled={saving} className="bg-destructive text-destructive-foreground hover:bg-destructive/90">删除</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function Metadata({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 rounded-lg bg-muted/50 p-3">
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="mt-1 truncate text-sm font-medium" title={value}>{value || "-"}</p>
    </div>
  );
}

function Message({ children, tone }: { children: React.ReactNode; tone: "error" | "warning" | "success" }) {
  const Icon = tone === "success" ? CheckCircle2 : AlertCircle;
  return (
    <div className={cn(
      "flex items-start gap-2 rounded-lg border px-3 py-2.5 text-sm",
      tone === "error" && "border-destructive/25 bg-destructive/10 text-destructive",
      tone === "warning" && "border-amber-500/25 bg-amber-500/10 text-amber-800 dark:text-amber-300",
      tone === "success" && "border-emerald-500/25 bg-emerald-500/10 text-emerald-800 dark:text-emerald-300",
    )}>
      <Icon className="mt-0.5 size-4 shrink-0" />
      <span>{children}</span>
    </div>
  );
}
