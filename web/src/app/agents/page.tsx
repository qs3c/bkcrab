"use client";

import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Textarea } from "@/components/ui/textarea";
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
import { Skeleton } from "@/components/ui/skeleton";
import { Bot, Plus, Trash2, ImagePlus, Pencil, Copy, Check } from "lucide-react";
import { Switch } from "@/components/ui/switch";
import {
  adminListAgents,
  apiFetch,
  getAgents,
  getMe,
  getStatus,
  createAgent,
  updateAgent,
  deleteAgent,
  type AgentDetail,
} from "@/lib/api";

interface OtherAgent {
  id: string;
  name: string;
  description?: string;
  userId: string;
  ownerUsername?: string;
  ownerEmail?: string;
  ownerDisplayName?: string;
}

// AgentAvatar tries to load /api/agents/{id}/files/avatar.png and falls
// back to the default Bot icon when the agent has no avatar yet (404).
function AgentAvatar({
  agent,
  bust,
  size = 48,
}: {
  agent: AgentDetail;
  bust?: number; // cache-buster ticked after upload
  size?: number;
}) {
  const [failed, setFailed] = useState(false);
  if (!agent.avatarUrl || failed) {
    return (
      <div
        className="flex shrink-0 items-center justify-center rounded-xl bg-primary/10 dark:bg-primary/15 border border-primary/15"
        style={{ width: size, height: size }}
      >
        <Bot className="text-primary" style={{ width: size * 0.5, height: size * 0.5 }} />
      </div>
    );
  }
  const url = bust ? `${agent.avatarUrl}?v=${bust}` : agent.avatarUrl;
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={url}
      alt={agent.name || agent.id}
      className="shrink-0 rounded-xl object-cover"
      style={{ width: size, height: size }}
      onError={() => setFailed(true)}
    />
  );
}

export default function AgentsPage() {
  const [agents, setAgents] = useState<AgentDetail[]>([]);
  const [otherAgents, setOtherAgents] = useState<OtherAgent[]>([]);
  const [isAdmin, setIsAdmin] = useState(false);
  const [loading, setLoading] = useState(true);
  const [activeTab, setActiveTab] = useState<"own" | "others">("own");
  // quotaLocked = true when the caller has agent_quota=0 (admin
  // provisions only). They can still browse /agents to see what's
  // been provisioned for them and jump into chat — we just hide the
  // Create button. If nothing has been provisioned yet, the empty
  // state tells them to contact their admin.
  const [quotaLocked, setQuotaLocked] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [editTarget, setEditTarget] = useState<AgentDetail | null>(null);
  const [deleteId, setDeleteId] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  // Bumped after avatar upload so <img> re-fetches the new file.
  const [avatarBust, setAvatarBust] = useState<Record<string, number>>({});

  // Create dialog state
  const [newName, setNewName] = useState("");
  const [newDescription, setNewDescription] = useState("");
  const [newAvatar, setNewAvatar] = useState<File | null>(null);
  const [newAvatarPreview, setNewAvatarPreview] = useState<string | null>(null);
  const [createError, setCreateError] = useState<string | null>(null);
  const createAvatarInput = useRef<HTMLInputElement>(null);

  // Edit dialog state
  const [editName, setEditName] = useState("");
  const [editDescription, setEditDescription] = useState("");
  const [editIsPublic, setEditIsPublic] = useState(false);
  const [editAvatar, setEditAvatar] = useState<File | null>(null);
  const [editAvatarPreview, setEditAvatarPreview] = useState<string | null>(null);
  const [editError, setEditError] = useState<string | null>(null);
  const [editLinkCopied, setEditLinkCopied] = useState(false);
  const editAvatarInput = useRef<HTMLInputElement>(null);

  const resetCreateForm = () => {
    setNewName("");
    setNewDescription("");
    setNewAvatar(null);
    if (newAvatarPreview) URL.revokeObjectURL(newAvatarPreview);
    setNewAvatarPreview(null);
    setCreateError(null);
  };

  const resetEditForm = () => {
    setEditName("");
    setEditDescription("");
    setEditIsPublic(false);
    setEditAvatar(null);
    if (editAvatarPreview) URL.revokeObjectURL(editAvatarPreview);
    setEditAvatarPreview(null);
    setEditError(null);
    setEditLinkCopied(false);
  };

  const openEdit = (agent: AgentDetail) => {
    resetEditForm();
    setEditTarget(agent);
    setEditName(agent.name || "");
    setEditDescription(agent.description || "");
    setEditIsPublic(!!agent.isPublic);
  };

  const fetchAgents = async () => {
    setLoading(true);
    // /api/agents returns the caller's owned agents only. Public agents
    // owned by other users surface as separate links — they don't auto-
    // populate the dashboard list.
    const list = await getAgents().catch(() => [] as AgentDetail[]);
    setAgents(list);
    // Admins also see other users' agents (read-only) below their own.
    // We resolve isAdmin from /api/status and only call adminListAgents
    // when entitled — non-admins would 403 and the UI would flash an error.
    const status = await getStatus().catch(() => null);
    const admin = !!status?.isAdmin;
    setIsAdmin(admin);
    if (admin) {
      const visibleIds = new Set(list.map((a) => a.id));
      const res = await adminListAgents().catch(() => null);
      const all: OtherAgent[] = (res?.agents || []) as OtherAgent[];
      setOtherAgents(all.filter((a) => !visibleIds.has(a.id)));
    } else {
      setOtherAgents([]);
    }
    setLoading(false);
  };

  const ownedAgents = agents;

  useEffect(() => {
    fetchAgents();
  }, []);

  // Resolve quotaLocked from /api/me. agent_quota === 0 means the
  // caller can't self-create — we hide the Create button but still
  // render the list so they can see admin-provisioned agents and
  // jump into chat.
  useEffect(() => {
    let aborted = false;
    getMe()
      .then((me) => {
        if (aborted) return;
        if (me?.user?.agentQuota === 0) setQuotaLocked(true);
      })
      .catch(() => {});
    return () => {
      aborted = true;
    };
  }, []);

  async function uploadAvatar(agentID: string, file: File) {
    const fd = new FormData();
    fd.append("file", file, "avatar.png");
    await apiFetch(`/api/agents/${agentID}/files`, { method: "POST", body: fd });
    setAvatarBust((m) => ({ ...m, [agentID]: Date.now() }));
  }

  const handleCreate = async () => {
    if (!newName.trim()) return;
    setSaving(true);
    setCreateError(null);
    const resp = await createAgent({
      name: newName.trim(),
      description: newDescription.trim() || undefined,
    });
    if (resp && (resp.ok === false || resp.error)) {
      setCreateError(resp.error || "创建智能体失败");
      setSaving(false);
      return;
    }
    const newId: string | undefined = resp?.agent?.id;
    if (newId && newAvatar) {
      try {
        await uploadAvatar(newId, newAvatar);
      } catch {
        // non-fatal — agent is created; avatar can be retried via Edit
      }
    }
    setSaving(false);
    setCreateOpen(false);
    resetCreateForm();
    fetchAgents();
  };

  const handleEdit = async () => {
    if (!editTarget || !editName.trim()) return;
    setSaving(true);
    setEditError(null);
    const resp = await updateAgent(editTarget.id, {
      name: editName.trim(),
      description: editDescription.trim(),
      isPublic: editIsPublic,
    });
    if (resp && (resp.ok === false || resp.error)) {
      setEditError(resp.error || "更新智能体失败");
      setSaving(false);
      return;
    }
    if (editAvatar) {
      try {
        await uploadAvatar(editTarget.id, editAvatar);
      } catch {
        // non-fatal — text fields saved; user can retry avatar upload
      }
    }
    setSaving(false);
    setEditTarget(null);
    resetEditForm();
    fetchAgents();
  };

  const handleDelete = async () => {
    if (!deleteId) return;
    await deleteAgent(deleteId);
    setDeleteId(null);
    fetchAgents();
  };

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">智能体</h2>
          <p className="text-sm text-muted-foreground mt-1">
            管理你的 AI 智能体及其配置
          </p>
        </div>
        {!quotaLocked && (
          <Button onClick={() => setCreateOpen(true)}>
            <Plus className="h-4 w-4 mr-2" />
            新建智能体
          </Button>
        )}
      </div>

      {loading ? (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-48" />
          ))}
        </div>
      ) : ownedAgents.length === 0 && otherAgents.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16 text-center">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <Bot className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground">
              {quotaLocked
                ? "你的账户尚未分配智能体，请联系管理员。"
                : "尚未配置智能体"}
            </p>
            {!quotaLocked && (
              <Button
                onClick={() => setCreateOpen(true)}
                variant="outline"
                className="mt-4"
              >
                创建你的第一个智能体
              </Button>
            )}
          </div>
        </div>
      ) : (
        <>
        {isAdmin && otherAgents.length > 0 && (
          <div className="flex gap-1 border-b border-border overflow-x-auto">
            <button
              onClick={() => setActiveTab("own")}
              className={`px-3 py-2 text-sm font-medium whitespace-nowrap border-b-2 transition-colors ${
                activeTab === "own"
                  ? "border-primary text-primary"
                  : "border-transparent text-muted-foreground hover:text-foreground"
              }`}
            >
              你的智能体
              <span className="ml-1.5 text-xs text-muted-foreground/70">
                {ownedAgents.length}
              </span>
            </button>
            <button
              onClick={() => setActiveTab("others")}
              className={`px-3 py-2 text-sm font-medium whitespace-nowrap border-b-2 transition-colors ${
                activeTab === "others"
                  ? "border-primary text-primary"
                  : "border-transparent text-muted-foreground hover:text-foreground"
              }`}
            >
              其他人的智能体
              <span className="ml-1.5 text-xs text-muted-foreground/70">
                {otherAgents.length}
              </span>
            </button>
          </div>
        )}
        {(activeTab === "own" || !(isAdmin && otherAgents.length > 0)) && ownedAgents.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {ownedAgents.map((agent) => (
            <div
              key={agent.id}
              className="group flex h-full flex-col rounded-lg border border-border bg-card p-5 transition-colors hover:bg-muted/50 cursor-pointer"
              onClick={() => (window.location.href = `/agents/${agent.id}/chat/`)}
            >
              <div className="flex items-start justify-between mb-4">
                <AgentAvatar agent={agent} bust={avatarBust[agent.id]} size={48} />
                {agent.isPublic ? (
                  <Badge
                    variant="outline"
                    className="bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border-emerald-500/20"
                  >
                    <span className="mr-1.5 inline-block h-1.5 w-1.5 rounded-full bg-emerald-500" />
                    公开
                  </Badge>
                ) : (
                  <Badge
                    variant="outline"
                    className="bg-muted/60 text-muted-foreground"
                  >
                    私有
                  </Badge>
                )}
              </div>
              <p className="text-base font-medium mb-1 truncate">{agent.name || agent.id}</p>
              <p
                className={`font-mono text-xs text-muted-foreground truncate ${
                  agent.description ? "" : "mb-3"
                }`}
              >
                {agent.id}
              </p>
              {agent.description && (
                <p className="mt-2 mb-3 text-sm text-muted-foreground line-clamp-2">
                  {agent.description}
                </p>
              )}
              {/* mt-auto pins the action row to the card bottom so cards
                  with no description don't shrink — keeps the grid row
                  aligned regardless of content length. */}
              {/* quotaLocked users (agent_quota=0) are admin-provisioned —
                  they can browse and chat but can't mutate the agent
                  record, so hide Edit/Remove entirely. */}
              {!quotaLocked && (
                <div className="flex items-center gap-2 mt-auto pt-3 border-t border-border">
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-8 text-xs"
                    onClick={(e) => {
                      e.stopPropagation();
                      openEdit(agent);
                    }}
                  >
                    <Pencil className="h-3 w-3 mr-1.5" />
                    编辑
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-8 text-xs text-destructive hover:text-destructive"
                    onClick={(e) => {
                      e.stopPropagation();
                      setDeleteId(agent.id);
                    }}
                  >
                    <Trash2 className="h-3 w-3 mr-1.5" />
                    移除
                  </Button>
                </div>
              )}
            </div>
          ))}
        </div>
        )}

        {isAdmin && otherAgents.length > 0 && activeTab === "others" && (
            <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
              {otherAgents.map((agent) => (
                <div
                  key={agent.id}
                  className="group flex h-full flex-col rounded-lg border border-border bg-card p-5 opacity-90 transition-colors hover:bg-muted/50 hover:opacity-100 cursor-pointer"
                  onClick={() =>
                    (window.location.href = `/agents/${agent.id}/chat/`)
                  }
                >
                  <div className="flex items-start justify-between mb-4">
                    <div className="flex shrink-0 items-center justify-center rounded-xl bg-gradient-to-br from-zinc-500 to-zinc-700 size-12">
                      <Bot className="text-white size-6" />
                    </div>
                    <Badge
                      variant="outline"
                      className="max-w-[60%] bg-muted/40 text-muted-foreground"
                    >
                      <span className="truncate">
                        所有者： {agent.ownerDisplayName || agent.ownerUsername || agent.userId}
                      </span>
                    </Badge>
                  </div>
                  <p className="text-base font-medium mb-1 truncate">
                    {agent.name || agent.id}
                  </p>
                  <p
                    className={`font-mono text-xs text-muted-foreground truncate ${
                      agent.description ? "" : "mb-3"
                    }`}
                  >
                    {agent.id}
                  </p>
                  {agent.description && (
                    <p className="mt-2 mb-3 text-sm text-muted-foreground line-clamp-2">
                      {agent.description}
                    </p>
                  )}
                  <div className="mt-auto pt-3 border-t border-border">
                    <p className="text-xs text-muted-foreground">
                      点击开始聊天。只有所有者可以编辑或移除此智能体。
                    </p>
                  </div>
                </div>
              ))}
            </div>
        )}
        </>
      )}

      {/* Create Dialog */}
      <Dialog
        open={createOpen}
        onOpenChange={(v) => {
          setCreateOpen(v);
          if (!v) resetCreateForm();
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>新建智能体</DialogTitle>
            <DialogDescription>
              系统会生成全局唯一 ID（例如{" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">agt_a1b2c3…</code>);
              下方内容仅用于显示。
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="flex items-start gap-4">
              <button
                type="button"
                onClick={() => createAvatarInput.current?.click()}
                className="group relative flex size-20 shrink-0 items-center justify-center overflow-hidden rounded-xl border border-dashed bg-muted/40 transition hover:bg-muted"
                aria-label="上传头像"
              >
                {newAvatarPreview ? (
                  // eslint-disable-next-line @next/next/no-img-element
                  <img src={newAvatarPreview} alt="avatar" className="size-full object-cover" />
                ) : (
                  <ImagePlus className="size-6 text-muted-foreground" />
                )}
                <input
                  ref={createAvatarInput}
                  type="file"
                  accept="image/*"
                  className="hidden"
                  onChange={(e) => {
                    const f = e.target.files?.[0] ?? null;
                    setNewAvatar(f);
                    if (newAvatarPreview) URL.revokeObjectURL(newAvatarPreview);
                    setNewAvatarPreview(f ? URL.createObjectURL(f) : null);
                  }}
                />
              </button>
              <div className="flex-1 space-y-2">
                <Label htmlFor="agent-name">名称</Label>
                <Input
                  id="agent-name"
                  value={newName}
                  onChange={(e) => {
                    setNewName(e.target.value);
                    setCreateError(null);
                  }}
                  placeholder="我的助手"
                  autoFocus
                />
              </div>
            </div>
            <div className="space-y-2">
              <Label htmlFor="agent-desc">描述（可选）</Label>
              <Textarea
                id="agent-desc"
                value={newDescription}
                onChange={(e) => setNewDescription(e.target.value)}
                placeholder="说明此智能体的用途，将显示在智能体列表和资料页中。"
                rows={3}
              />
            </div>
            {createError && (
              <p className="text-sm text-destructive">{createError}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)}>
              取消
            </Button>
            <Button onClick={handleCreate} disabled={!newName.trim() || saving}>
              {saving ? "正在创建..." : "创建智能体"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Edit Dialog */}
      <Dialog
        open={editTarget !== null}
        onOpenChange={(v) => {
          if (!v) {
            setEditTarget(null);
            resetEditForm();
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>编辑智能体</DialogTitle>
            <DialogDescription>
              ID 已锁定：{" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
                {editTarget?.id}
              </code>
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="flex items-start gap-4">
              <button
                type="button"
                onClick={() => editAvatarInput.current?.click()}
                className="group relative flex size-20 shrink-0 items-center justify-center overflow-hidden rounded-xl border border-dashed bg-muted/40 transition hover:bg-muted"
                aria-label="上传头像"
              >
                {editAvatarPreview ? (
                  // eslint-disable-next-line @next/next/no-img-element
                  <img src={editAvatarPreview} alt="avatar" className="size-full object-cover" />
                ) : editTarget ? (
                  <AgentAvatar agent={editTarget} bust={avatarBust[editTarget.id]} size={80} />
                ) : null}
                <input
                  ref={editAvatarInput}
                  type="file"
                  accept="image/*"
                  className="hidden"
                  onChange={(e) => {
                    const f = e.target.files?.[0] ?? null;
                    setEditAvatar(f);
                    if (editAvatarPreview) URL.revokeObjectURL(editAvatarPreview);
                    setEditAvatarPreview(f ? URL.createObjectURL(f) : null);
                  }}
                />
              </button>
              <div className="flex-1 space-y-2">
                <Label htmlFor="agent-edit-name">名称</Label>
                <Input
                  id="agent-edit-name"
                  value={editName}
                  onChange={(e) => {
                    setEditName(e.target.value);
                    setEditError(null);
                  }}
                  placeholder="我的助手"
                />
              </div>
            </div>
            <div className="space-y-2">
              <Label htmlFor="agent-edit-desc">描述</Label>
              <Textarea
                id="agent-edit-desc"
                value={editDescription}
                onChange={(e) => setEditDescription(e.target.value)}
                placeholder="这个智能体用于什么？"
                rows={3}
              />
            </div>

            {/* Public/Private toggle. Off (default) = owner-only.
                On = anyone with the chat URL can chat under their own
                account; sessions/memory partition per chatter. */}
            <div className="space-y-3 rounded-lg border border-border p-4">
              <div className="flex items-start justify-between gap-4">
                <div className="space-y-1">
                  <Label htmlFor="agent-edit-public" className="text-sm font-medium">
                    公开访问
                  </Label>
                  <p className="text-xs text-muted-foreground">
                    {editIsPublic
                      ? "任何获得链接的人都可以聊天，其对话历史仅本人可见。"
                      : "只有你可以使用此智能体。"}
                  </p>
                </div>
                <Switch
                  id="agent-edit-public"
                  checked={editIsPublic}
                  onCheckedChange={(v) => {
                    setEditIsPublic(!!v);
                    setEditLinkCopied(false);
                  }}
                />
              </div>
              {editIsPublic && editTarget && (
                <div className="flex gap-2">
                  <Input
                    readOnly
                    value={
                      typeof window !== "undefined"
                        ? `${window.location.origin}/agents/${editTarget.id}/chat/`
                        : `/agents/${editTarget.id}/chat/`
                    }
                    onFocus={(e) => e.currentTarget.select()}
                    className="font-mono text-xs"
                  />
                  <Button
                    type="button"
                    variant="outline"
                    onClick={async () => {
                      if (!editTarget) return;
                      const url = `${window.location.origin}/agents/${editTarget.id}/chat/`;
                      try {
                        await navigator.clipboard.writeText(url);
                        setEditLinkCopied(true);
                        setTimeout(() => setEditLinkCopied(false), 2000);
                      } catch {
                        // clipboard blocked — user can still select the input
                      }
                    }}
                  >
                    {editLinkCopied ? (
                      <>
                        <Check className="h-4 w-4 mr-1.5" />
                        已复制
                      </>
                    ) : (
                      <>
                        <Copy className="h-4 w-4 mr-1.5" />
                        复制
                      </>
                    )}
                  </Button>
                </div>
              )}
            </div>

            {editError && <p className="text-sm text-destructive">{editError}</p>}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditTarget(null)}>
              取消
            </Button>
            <Button onClick={handleEdit} disabled={!editName.trim() || saving}>
              {saving ? "正在保存..." : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete Confirmation */}
      <AlertDialog open={!!deleteId} onOpenChange={() => setDeleteId(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除智能体</AlertDialogTitle>
            <AlertDialogDescription>
              确定要删除 <strong>{deleteId}</strong>?
              此操作无法撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

    </div>
  );
}
