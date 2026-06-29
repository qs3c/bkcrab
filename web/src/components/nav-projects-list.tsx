"use client";

import * as React from "react";
import { useRouter, usePathname } from "next/navigation";
import {
  SidebarGroup,
  SidebarGroupAction,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuAction,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  useSidebar,
} from "@/components/ui/sidebar";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
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
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import {
  ChevronRightIcon,
  FolderIcon,
  FolderOpenIcon,
  MoreHorizontalIcon,
  PencilIcon,
  PlusIcon,
  Trash2Icon,
} from "lucide-react";
import {
  createProject,
  deleteProject,
  moveChatSessionToProject,
  updateProject,
  type ProjectEntry,
} from "@/lib/api";
import {
  CHAT_DRAG_MIME,
  SessionStatusIndicator,
  hasChatPayload,
  type SessionItem,
} from "@/components/nav-projects";
import { ChatRowActions } from "@/components/chat-row-actions";

// NavProjectsList 是智能体侧边栏的"项目"部分。每个项目
// 内联展开显示其子对话；点击项目内的"+ 新建对话"会创建一个
// 预绑定到该项目的新会话，使第一次对话直接写入 projects/<pid>/。
//
// `sessions` 是完整的每个智能体会话列表 —— 我们按 projectId 过滤到
// 每个项目。扁平的 NavSessions 组件负责在己方过滤掉项目会话，
// 避免它们在下方"对话"部分重复渲染。

export interface ProjectChatItem extends SessionItem {
  projectId?: string;
}

export function NavProjectsList({
  agentId,
  projects,
  sessions,
  onChanged,
}: {
  agentId: string | null;
  projects: ProjectEntry[];
  sessions: ProjectChatItem[];
  // 调用者在此触发时重新拉取项目和会话（重命名、删除、创建对话）。
  // 与 NavSessions 广播的 `bkclaw:sessions-changed` 事件模式相同；
  // 我们保留为回调属性，因为 Project 状态住在上一层 AppSidebar 中。
  onChanged: () => void;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const [createOpen, setCreateOpen] = React.useState(false);
  const [editTarget, setEditTarget] = React.useState<ProjectEntry | null>(null);
  const [deleteTarget, setDeleteTarget] = React.useState<ProjectEntry | null>(
    null,
  );
  // 已展开的项目 ID 集合：点击项目切换其展开状态。
  // 使用 Set 以便多个项目可以同时展开。
  const [expanded, setExpanded] = React.useState<Set<string>>(new Set());

  // useMemo 必须在下方的 early-return 之前运行，否则活跃智能体
  // 出现/消失时 hook 顺序会在渲染间变化。
  const sessionsByProject = React.useMemo(() => {
    const m = new Map<string, ProjectChatItem[]>();
    for (const s of sessions) {
      if (!s.projectId) continue;
      const arr = m.get(s.projectId) ?? [];
      arr.push(s);
      m.set(s.projectId, arr);
    }
    return m;
  }, [sessions]);

  // 两个不同的"活跃项目"信号 —— 它们看起来相似但门控不同的事物，
  // 将它们放在一起解析曾隐藏一个微妙的 bug：从项目的某个会话点击
  // 项目会变成切换而非导航：
  //
  //   urlProjectId  —— 仅当 URL 路径恰好是项目的空白新对话状态
  //                   `/agents/<aid>/project/<pid>/`（无会话）时设置。
  //                   驱动下方的 点击-切换 分支：只有当用户已坐在
  //                   项目的着陆 URL 上时，点击才是"切换"。
  //                   其他任何位置（包括属于此项目的会话中）点击
  //                   都是导航。
  //
  //   activeProjectId —— 更广泛：也匹配 URL 在某个 project_id
  //                     为此项目的会话上时。纯用于视觉/展开提示 ——
  //                     该行获得 `isActive` 高亮并自动展开，使用户
  //                     在阅读任何项目对话时始终看到其项目的对话。
  const projectPathMatch = pathname.match(/\/agents\/[^/]+\/project\/([^/]+)\/?$/);
  const urlProjectId = projectPathMatch ? projectPathMatch[1] : null;
  const sessionPathMatch = pathname.match(/\/agents\/[^/]+\/chat\/([^/]+)\/?$/);
  const urlSessionId = sessionPathMatch ? sessionPathMatch[1] : null;
  const activeProjectId = React.useMemo(() => {
    if (urlProjectId) return urlProjectId;
    if (urlSessionId) {
      const sess = sessions.find((s) => s.id === urlSessionId);
      if (sess?.projectId) return sess.projectId;
    }
    return null;
  }, [urlProjectId, urlSessionId, sessions]);

  // 自动展开活跃项目，使用户着陆到项目对话 URL 时总能看到
  // 当前项目的对话 —— 没有此项，在 `?project=<pid>` 上全新页面
  // 加载会显示项目折叠并需要额外一次点击。只添加；我们从不在
  // 自动模式下收起，因此用户的主动收起会持久保留。
  React.useEffect(() => {
    if (!activeProjectId) return;
    setExpanded((prev) => {
      if (prev.has(activeProjectId)) return prev;
      const next = new Set(prev);
      next.add(activeProjectId);
      return next;
    });
  }, [activeProjectId]);

  // 追踪我们上次请求路由器导航到、但尚未在 `pathname` 中观察到的 URL。
  // 对同一项目标题的快速双击/三击曾导致每次点击堆叠一个 `router.push`；
  // 在静态导出下每次 push 触发自己的 RSC 请求，堆积足够多会耗尽浏览器
  // 每源 6 连接池，导致后续点击（和进行中的 SSE）永远处于 `pending` 状态。
  // hook 必须在下方 early-return 之前运行以保持各次渲染间 hook 调用顺序稳定。
  const inFlightTargetRef = React.useRef<string | null>(null);
  React.useEffect(() => {
    inFlightTargetRef.current = null;
  }, [pathname]);
  const navigateOnce = React.useCallback(
    (target: string) => {
      const here =
        pathname === target || pathname === target.replace(/\/$/, "");
      if (here) return;
      if (inFlightTargetRef.current === target) return;
      inFlightTargetRef.current = target;
      router.push(target);
    },
    [pathname, router],
  );

  if (!agentId) return null;

  const chatBase = `/agents/${agentId}/chat/`;
  const projectBase = `/agents/${agentId}/project/`;
  // 路径格式：/chat/<sid>/。从 pathname 读取，使高亮与 URL 保持
  // 同步（旧的 `?session=` 代码路径依赖非响应式的
  // `window.location.search` 读取）。
  const activeSessionKey = urlSessionId;

  const toggleExpand = (id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  // onProjectClick：点击项目标题总是导航到项目的空白新对话 URL
  //（/project/<pid>/）—— 即使从属于同一项目的会话中也如此，
  // 因此点击是可靠的"在此项目中给我一个新对话"的操作入口。
  // 切换行为仅在用户坐在该确切 URL 上时生效，因此从项目自身
  // 着陆页对项目的第二次点击会折叠/重新打开该行。
  const onProjectClick = (projectId: string) => {
    if (urlProjectId === projectId) {
      toggleExpand(projectId);
      return;
    }
    navigateOnce(`${projectBase}${encodeURIComponent(projectId)}/`);
  };

  const startNewChat = (projectId: string) => {
    // 由"…"下拉菜单的"在项目中新建对话"项使用 —— 与标题点击
    // 相同的导航，但跳过"当前在项目中 = 切换"分支，因此总是
    // 打开一个新对话。
    navigateOnce(`${projectBase}${encodeURIComponent(projectId)}/`);
  };

  return (
    <>
      <SidebarGroup className="group-data-[collapsible=icon]:hidden">
        <SidebarGroupLabel>项目</SidebarGroupLabel>
        <SidebarGroupAction
          aria-label="新建项目"
          onClick={() => setCreateOpen(true)}
          render={
            <button>
              <PlusIcon className="size-4" />
            </button>
          }
        />
        <SidebarMenu>
          {projects.length === 0 && (
            <SidebarMenuItem>
              <div className="px-2 py-1.5 text-xs text-muted-foreground">
                暂无项目
              </div>
            </SidebarMenuItem>
          )}
          {projects.map((p) => {
            const isOpen = expanded.has(p.id);
            const projectSessions = sessionsByProject.get(p.id) ?? [];
            return (
              <ProjectRow
                key={p.id}
                project={p}
                open={isOpen}
                isActive={activeProjectId === p.id}
                onClick={() => onProjectClick(p.id)}
                onEdit={() => setEditTarget(p)}
                onDelete={() => setDeleteTarget(p)}
                onNewChat={() => startNewChat(p.id)}
                sessions={projectSessions}
                activeSessionKey={activeSessionKey}
                onOpenSession={(sid) =>
                  navigateOnce(`${chatBase}${encodeURIComponent(sid)}/`)
                }
                isOnChatRoute={pathname.startsWith(chatBase)}
                allSessions={sessions}
                agentId={agentId}
                onMoved={() => {
                  // 拖放后自动展开目标项目，使用户立即看到对话落入新位置，
                  // 然后触发重新拉取。
                  setExpanded((prev) => {
                    if (prev.has(p.id)) return prev;
                    const next = new Set(prev);
                    next.add(p.id);
                    return next;
                  });
                  onChanged();
                }}
              />
            );
          })}
        </SidebarMenu>
      </SidebarGroup>

      <CreateProjectDialog
        open={createOpen}
        agentId={agentId}
        onOpenChange={(v) => setCreateOpen(v)}
        onCreated={onChanged}
      />
      <EditProjectDialog
        target={editTarget}
        agentId={agentId}
        onClose={() => setEditTarget(null)}
        onSaved={onChanged}
      />
      <DeleteProjectDialog
        target={deleteTarget}
        agentId={agentId}
        onClose={() => setDeleteTarget(null)}
        onDeleted={onChanged}
      />
    </>
  );
}

function ProjectRow({
  project,
  open,
  isActive,
  onClick,
  onEdit,
  onDelete,
  onNewChat,
  sessions,
  activeSessionKey,
  onOpenSession,
  isOnChatRoute,
  allSessions,
  agentId,
  onMoved,
}: {
  project: ProjectEntry;
  open: boolean;
  // isActive 标记用户当前正在查看的项目 —— 驱动标题上的
  // 视觉选中状态和点击语义（活跃 = 切换，非活跃 = 导航）。
  isActive: boolean;
  onClick: () => void;
  onEdit: () => void;
  onDelete: () => void;
  // onNewChat 现在仅通过"…"下拉菜单连接 —— 展开的子列表不再
  // 提供"+ 新建对话"入口，因为点击项目标题本身会在项目中打开
  // 一个新对话（`?project=<pid>` URL = 空白新对话状态）。
  onNewChat: () => void;
  sessions: ProjectChatItem[];
  activeSessionKey: string | null;
  onOpenSession: (sessionId: string) => void;
  isOnChatRoute: boolean;
  // allSessions 是完整的每个智能体列表 —— 在拖放期间需要查找
  // 源对话的当前 projectId，使自我拖放成为廉价的空操作而非
  // 浪费一次 API 往返。
  allSessions: ProjectChatItem[];
  agentId: string;
  onMoved: () => void;
}) {
  const { isMobile } = useSidebar();
  const [dropActive, setDropActive] = React.useState(false);
  const onDragOver = (e: React.DragEvent) => {
    if (!hasChatPayload(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
    if (!dropActive) setDropActive(true);
  };
  const onDragLeave = () => setDropActive(false);
  const onDrop = async (e: React.DragEvent) => {
    if (!hasChatPayload(e)) return;
    e.preventDefault();
    // 阻止父级"对话"组也处理此拖放 —— 没有此行则项目上的拖放
    // 也会触发松散对话的解绑处理器。
    e.stopPropagation();
    setDropActive(false);
    const sid = e.dataTransfer.getData(CHAT_DRAG_MIME);
    if (!sid) return;
    const sess = allSessions.find((s) => s.id === sid);
    if (sess && sess.projectId === project.id) return;
    const res = await moveChatSessionToProject(agentId, sid, project.id);
    if (res?.error) {
      console.error("move chat to project failed:", res.error);
      window.alert(`移动会话失败：${res.error}`);
      return;
    }
    onMoved();
  };
  return (
    <SidebarMenuItem
      onDragOver={onDragOver}
      onDragLeave={onDragLeave}
      onDrop={onDrop}
      className={dropActive ? "rounded-md outline outline-2 outline-primary/40" : ""}
    >
      <SidebarMenuButton
        tooltip={project.name}
        isActive={isActive}
        onClick={onClick}
        className="font-medium"
      >
        {/* 默认：文件夹图标。悬停：切换为箭头，让用户知道行可折叠
            （箭头的旋转传达当前展开/折叠状态）。两个图标共享同一
            位置，避免悬停切换时行宽跳动。group/menu-item 在
            components/ui/sidebar.tsx 中设置于 SidebarMenuItem 上。 */}
        <span className="relative size-4 shrink-0">
          <span className="absolute inset-0 flex items-center justify-center transition-opacity group-hover/menu-item:opacity-0">
            {open ? <FolderOpenIcon /> : <FolderIcon />}
          </span>
          <span className="absolute inset-0 flex items-center justify-center opacity-0 transition-opacity group-hover/menu-item:opacity-100">
            <ChevronRightIcon
              className={
                "transition-transform " + (open ? "rotate-90" : "rotate-0")
              }
            />
          </span>
        </span>
        <span className="truncate">{project.name}</span>
      </SidebarMenuButton>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <SidebarMenuAction showOnHover>
              <MoreHorizontalIcon />
              <span className="sr-only">项目操作</span>
            </SidebarMenuAction>
          }
        />
        <DropdownMenuContent
          className="w-44 rounded-lg"
          side={isMobile ? "bottom" : "right"}
          align={isMobile ? "end" : "start"}
        >
          <DropdownMenuItem onClick={onNewChat}>
            <PlusIcon className="text-muted-foreground" />
            <span>在项目中新建对话</span>
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onClick={onEdit}>
            <PencilIcon className="text-muted-foreground" />
            <span>编辑</span>
          </DropdownMenuItem>
          <DropdownMenuItem
            onClick={onDelete}
            className="text-destructive focus:text-destructive"
          >
            <Trash2Icon className="text-destructive" />
            <span>删除</span>
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      {open && sessions.length > 0 && (
        <SidebarMenuSub>
          {sessions.map((s) => {
            const active = isOnChatRoute && activeSessionKey === s.id;
            return (
              <SidebarMenuSubItem
                key={s.id}
                draggable
                onDragStart={(e) => {
                  e.dataTransfer.setData(CHAT_DRAG_MIME, s.id);
                  e.dataTransfer.effectAllowed = "move";
                }}
              >
                <SidebarMenuSubButton
                  isActive={active}
                  onClick={(e) => {
                    e.preventDefault();
                    onOpenSession(s.id);
                  }}
                  // 右侧预留空间，使标题不会在悬停时滑到绝对定位的操作芯片下方。
                   // pr-7 ≈ 5 单位芯片宽度 + 间距。
                  className="pr-7"
                >
                  <span className="flex min-w-0 flex-1 items-center gap-1.5">
                    <span className="truncate">{s.title || s.id}</span>
                    <SessionStatusIndicator status={s.lastTurnStatus} />
                  </span>
                </SidebarMenuSubButton>
                <ChatRowActions
                  agentId={agentId}
                  session={{ id: s.id, title: s.title }}
                  onChanged={onMoved}
                  variant="menu-sub-item"
                />
              </SidebarMenuSubItem>
            );
          })}
        </SidebarMenuSub>
      )}
    </SidebarMenuItem>
  );
}

function CreateProjectDialog({
  open,
  agentId,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  agentId: string;
  onOpenChange: (v: boolean) => void;
  onCreated: () => void;
}) {
  const [name, setName] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (open) {
      setName("");
      setDescription("");
    }
  }, [open]);

  const save = async () => {
    const n = name.trim();
    if (!n) return;
    setSaving(true);
    try {
      const res = await createProject(agentId, {
        name: n,
        description: description.trim() || undefined,
      });
      if ("error" in res && res.error) return;
      onCreated();
      onOpenChange(false);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>新建项目</DialogTitle>
          <DialogDescription>
            将共享研究资料、文件或上下文的对话归为一组。同一项目内的所有对话共用工作区文件夹。
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div>
            <label className="mb-1 block text-xs font-medium">名称</label>
            <Input
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="例如 NLP 调研"
            />
          </div>
          <div>
            <label className="mb-1 block text-xs font-medium">
              描述（可选）
            </label>
            <Textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="这个项目用于什么…"
              rows={3}
            />
          </div>
        </div>
        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={saving}
          >
            取消
          </Button>
          <Button onClick={save} disabled={saving || !name.trim()}>
            {saving ? "正在创建…" : "创建"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function EditProjectDialog({
  target,
  agentId,
  onClose,
  onSaved,
}: {
  target: ProjectEntry | null;
  agentId: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    setName(target?.name ?? "");
    setDescription(target?.description ?? "");
  }, [target]);

  if (!target) return null;

  const save = async () => {
    const n = name.trim();
    if (!n) return;
    setSaving(true);
    try {
      const res = await updateProject(agentId, target.id, {
        name: n,
        description: description.trim(),
      });
      if ("error" in res && res.error) return;
      onSaved();
      onClose();
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog open={!!target} onOpenChange={(v) => !v && onClose()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>编辑项目</DialogTitle>
          <DialogDescription>
            重命名或更新描述。工作区文件夹保持不变，文件不会移动。
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div>
            <label className="mb-1 block text-xs font-medium">名称</label>
            <Input
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <div>
            <label className="mb-1 block text-xs font-medium">
              描述
            </label>
            <Textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={3}
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={saving}>
            取消
          </Button>
          <Button onClick={save} disabled={saving || !name.trim()}>
            {saving ? "正在保存…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function DeleteProjectDialog({
  target,
  agentId,
  onClose,
  onDeleted,
}: {
  target: ProjectEntry | null;
  agentId: string;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const [error, setError] = React.useState<string>("");
  const [busy, setBusy] = React.useState(false);

  React.useEffect(() => {
    if (target) setError("");
  }, [target]);

  const onConfirm = async () => {
    if (!target) return;
    setBusy(true);
    try {
      const res = await deleteProject(agentId, target.id);
      if (res.error) {
        // 服务器在项目仍拥有对话时返回 409 并带 sessionCount ——
        // 显示提示而非仅仅"删除失败"。
        if (res.sessionCount && res.sessionCount > 0) {
          setError(
            `This 项目 still has ${res.sessionCount} 对话${res.sessionCount === 1 ? "" : "s"}. 删除 or move them first.`,
          );
        } else {
          setError(res.error);
        }
        return;
      }
      onDeleted();
      onClose();
    } finally {
      setBusy(false);
    }
  };

  return (
    <AlertDialog open={!!target} onOpenChange={(v) => !v && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>删除项目</AlertDialogTitle>
          <AlertDialogDescription>
            删除 <strong>{target?.name}</strong>？必须先移除项目内的对话，删除项目不会级联删除对话。磁盘上的工作区文件夹会保留。
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error && (
          <div className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-xs text-destructive">
            {error}
          </div>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
          <AlertDialogAction
            onClick={onConfirm}
            disabled={busy}
            className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
          >
            {busy ? "正在删除…" : "删除"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
