"use client";

import * as React from "react";
import {
  AlertCircle,
  CheckCircle2,
  ChevronRight,
  Database,
  FileText,
  Files,
  Loader2,
  Pencil,
  Plus,
  RefreshCw,
  Search,
  Trash2,
  Upload,
  XCircle,
} from "lucide-react";

import {
  createKnowledgeBase,
  deleteKnowledgeBase,
  deleteKnowledgeDocument,
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
} from "@/lib/api";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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
const ACCEPTED_EXTENSIONS = [".md", ".txt", ".pdf", ".docx"];
const MAX_FILE_BYTES = 20 * 1024 * 1024;

type KBForm = {
  name: string;
  description: string;
  chunkSize: string;
  chunkOverlap: string;
};

const EMPTY_FORM: KBForm = {
  name: "",
  description: "",
  chunkSize: String(DEFAULT_CHUNK_SIZE),
  chunkOverlap: String(DEFAULT_CHUNK_OVERLAP),
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

function documentStatus(status: string) {
  switch (status.toUpperCase()) {
    case "DONE":
      return {
        label: "已索引",
        icon: CheckCircle2,
        className: "border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
      };
    case "FAILED":
      return {
        label: "失败",
        icon: XCircle,
        className: "border-destructive/25 bg-destructive/10 text-destructive",
      };
    case "PROCESSING":
      return {
        label: "处理中",
        icon: Loader2,
        className: "border-blue-500/25 bg-blue-500/10 text-blue-700 dark:text-blue-400",
      };
    default:
      return {
        label: "等待处理",
        icon: Loader2,
        className: "border-amber-500/25 bg-amber-500/10 text-amber-700 dark:text-amber-400",
      };
  }
}

export default function KnowledgePage() {
  const [knowledgeBases, setKnowledgeBases] = React.useState<KnowledgeBase[]>([]);
  const [selectedId, setSelectedId] = React.useState("");
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

  const loadKnowledgeBases = React.useCallback(async (preferredId?: string) => {
    setLoading(true);
    setError("");
    try {
      const rows = await listKnowledgeBases();
      setKnowledgeBases(rows);
      setSelectedId((current) => {
        const desired = preferredId || current;
        return rows.some((kb) => kb.id === desired) ? desired : rows[0]?.id || "";
      });
    } catch (err) {
      setKnowledgeBases([]);
      setSelectedId("");
      setError(errorMessage(err, "读取知识库失败"));
    } finally {
      setLoading(false);
    }
  }, []);

  const loadDocuments = React.useCallback(async (kbId: string, quiet = false) => {
    if (!quiet) setDocumentsLoading(true);
    try {
      const rows = await listKnowledgeDocuments(kbId);
      setDocuments(rows);
    } catch (err) {
      if (!quiet) setError(errorMessage(err, "读取文档失败"));
    } finally {
      if (!quiet) setDocumentsLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void loadKnowledgeBases();
    void getMe().then((me) => setReadOnly(me.readOnly === true)).catch(() => undefined);
  }, [loadKnowledgeBases]);

  React.useEffect(() => {
    setDocuments([]);
    setHits([]);
    setSearched(false);
    if (selectedId) void loadDocuments(selectedId);
  }, [selectedId, loadDocuments]);

  const indexing = documents.some((doc) =>
    ["PENDING", "PROCESSING"].includes(doc.status.toUpperCase()),
  );

  React.useEffect(() => {
    if (!selectedId || !indexing) return;
    const timer = window.setInterval(() => void loadDocuments(selectedId, true), 2500);
    return () => window.clearInterval(timer);
  }, [selectedId, indexing, loadDocuments]);

  const openCreate = () => {
    setEditing(null);
    setForm(EMPTY_FORM);
    setFormError("");
    setFormOpen(true);
  };

  const openEdit = (kb: KnowledgeBase) => {
    setEditing(kb);
    setForm({
      name: kb.name,
      description: kb.description,
      chunkSize: String(kb.chunkSize),
      chunkOverlap: String(kb.chunkOverlap),
    });
    setFormError("");
    setFormOpen(true);
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
      const payload = {
        name,
        description: form.description.trim(),
        chunkSize,
        chunkOverlap,
      };
      const saved = editing
        ? await updateKnowledgeBase(editing.id, payload)
        : await createKnowledgeBase(payload);
      setFormOpen(false);
      setNotice(editing ? "知识库设置已保存" : "知识库已创建，可以开始上传文档");
      window.setTimeout(() => setNotice(""), 2500);
      await loadKnowledgeBases(saved.id);
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
    const invalid = files.find((file) => {
      const lower = file.name.toLowerCase();
      return !ACCEPTED_EXTENSIONS.some((extension) => lower.endsWith(extension));
    });
    if (invalid) {
      setError(`${invalid.name} 的格式不受支持，请上传 MD、TXT、PDF 或 DOCX`);
      return;
    }
    const tooLarge = files.find((file) => file.size > MAX_FILE_BYTES);
    if (tooLarge) {
      setError(`${tooLarge.name} 超过 20 MB 大小限制`);
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
                新建一个知识库，上传 MD、TXT、PDF 或 DOCX 文档后即可让智能体检索。
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
                  onClick={() => setSelectedId(kb.id)}
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
                    <Button variant="ghost" size="icon" onClick={() => openEdit(selected)} disabled={readOnly} aria-label="编辑知识库">
                      <Pencil className="size-4" />
                    </Button>
                    <Button variant="ghost" size="icon" onClick={() => setDeleteKBTarget(selected)} disabled={readOnly} aria-label="删除知识库" className="text-muted-foreground hover:text-destructive">
                      <Trash2 className="size-4" />
                    </Button>
                  </CardAction>
                </CardHeader>
                <CardContent className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
                  <Metadata label="Embedding 模型" value={selected.embedModel} />
                  <Metadata label="向量维度" value={`${selected.embedDims} 维`} />
                  <Metadata label="分片长度" value={`${selected.chunkSize} tokens`} />
                  <Metadata label="重叠长度" value={`${selected.chunkOverlap} tokens`} />
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
                      accept={ACCEPTED_EXTENSIONS.join(",")}
                      multiple
                      onChange={(event) => void uploadFiles(Array.from(event.target.files || []))}
                    />
                    <Button variant="outline" size="sm" onClick={() => fileInputRef.current?.click()} disabled={readOnly || uploading}>
                      {uploading ? <Loader2 className="size-4 animate-spin" /> : <Upload className="size-4" />}
                      {uploading ? "上传中" : "上传文档"}
                    </Button>
                  </CardAction>
                </CardHeader>
                <CardContent className="space-y-4">
                  <button
                    type="button"
                    disabled={readOnly || uploading}
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
                      (readOnly || uploading) && "cursor-not-allowed opacity-60",
                    )}
                  >
                    {uploading ? <Loader2 className="mb-2 size-6 animate-spin text-primary" /> : <Upload className="mb-2 size-6 text-muted-foreground" />}
                    <span className="text-sm font-medium">{uploadProgress || "拖放文件到这里，或点击选择"}</span>
                    <span className="mt-1 text-xs text-muted-foreground">支持 MD、TXT、PDF、DOCX，单文件最大 20 MB</span>
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
                            const status = documentStatus(doc.status);
                            const StatusIcon = status.icon;
                            return (
                              <TableRow key={doc.id}>
                                <TableCell>
                                  <div className="flex min-w-[190px] items-center gap-3">
                                    <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-muted"><FileText className="size-4" /></span>
                                    <span className="min-w-0">
                                      <span className="block max-w-[260px] truncate text-sm font-medium" title={doc.fileName}>{doc.fileName}</span>
                                      <span className="text-xs text-muted-foreground">{formatBytes(doc.fileSize)} · v{doc.version}</span>
                                    </span>
                                  </div>
                                  {doc.errorMsg && <p className="mt-1 max-w-[320px] text-xs text-destructive">{doc.errorMsg}</p>}
                                </TableCell>
                                <TableCell>
                                  <Badge variant="outline" className={status.className}>
                                    <StatusIcon className={cn("size-3", ["PENDING", "PROCESSING"].includes(doc.status.toUpperCase()) && "animate-spin")} />
                                    {status.label}
                                  </Badge>
                                </TableCell>
                                <TableCell className="whitespace-nowrap text-sm text-muted-foreground">
                                  {doc.status.toUpperCase() === "DONE" ? `${doc.chunkCount} 段 · ${doc.tokenCount} tokens` : "-"}
                                </TableCell>
                                <TableCell className="whitespace-nowrap text-sm text-muted-foreground">{formatTime(doc.uploadedAt)}</TableCell>
                                <TableCell>
                                  <div className="flex justify-end gap-1">
                                    <Button variant="ghost" size="icon" aria-label="重新索引" title="重新索引" onClick={() => void reindexDocument(doc)} disabled={readOnly || ["PENDING", "PROCESSING"].includes(doc.status.toUpperCase())}>
                                      <RefreshCw className="size-4" />
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

      <Dialog open={formOpen} onOpenChange={setFormOpen}>
        <DialogContent className="sm:max-w-lg">
          <form onSubmit={saveKnowledgeBase}>
            <DialogHeader>
              <DialogTitle>{editing ? "编辑知识库" : "新建知识库"}</DialogTitle>
              <DialogDescription>
                分片单位是 token。创建后会绑定当前 embedding 模型与向量维度。
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4 py-5">
              {formError && <Message tone="error">{formError}</Message>}
              <div className="space-y-2">
                <Label htmlFor="kb-name">名称</Label>
                <Input id="kb-name" autoFocus value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} placeholder="例如：产品与售后资料" maxLength={120} />
              </div>
              <div className="space-y-2">
                <Label htmlFor="kb-description">描述</Label>
                <Textarea id="kb-description" value={form.description} onChange={(event) => setForm((current) => ({ ...current, description: event.target.value }))} placeholder="说明这个知识库包含哪些内容" rows={3} />
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
              {editing && (
                <div className="rounded-lg bg-muted/60 p-3 text-xs text-muted-foreground">
                  已绑定 {editing.embedModel}（{editing.embedDims} 维）。修改分片参数后，已有文档需要重新索引才会生效。
                </div>
              )}
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setFormOpen(false)}>取消</Button>
              <Button type="submit" disabled={saving}>
                {saving && <Loader2 className="size-4 animate-spin" />}
                {saving ? "保存中" : "保存"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

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
