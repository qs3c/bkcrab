"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
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
import { useSidebar } from "@/components/ui/sidebar";
import { MoreHorizontalIcon, PencilIcon, Trash2Icon } from "lucide-react";
import { deleteChatSession, renameChatSession } from "@/lib/api";

// ChatRowActions 是附加在侧边栏每个聊天行上的共享"…"下拉菜单
// — 包括扁平"对话"列表和项目下嵌套的对话。它自行管理编辑/删除
// 对话框的状态，调用者只需渲染触发器，对话框在关闭时立即卸载。
//
// variant 控制定位和悬停门控：
//   "menu-item"     — 配合 SidebarMenuButton（top-1.5 right-1）
//   "menu-sub-item" — 配合 SidebarMenuSubButton（更小；使用
//                     group/menu-sub-item 悬停作用域，触发器仅在
//                     子行被悬停时淡入）。

export interface ChatRowSession {
  id: string;
  title: string;
}

export function ChatRowActions({
  agentId,
  session,
  onChanged,
  variant = "menu-item",
}: {
  agentId: string;
  session: ChatRowSession;
  onChanged: () => void;
  variant?: "menu-item" | "menu-sub-item";
}) {
  const router = useRouter();
  const { isMobile } = useSidebar();
  const [editOpen, setEditOpen] = React.useState(false);
  const [deleteOpen, setDeleteOpen] = React.useState(false);

  const onConfirmDelete = async () => {
    setDeleteOpen(false);
    try {
      await deleteChatSession(agentId, session.id);
    } finally {
      // 如果删除的会话当前已打开，则跳转回新聊天 URL，
      // 避免页面停留在过期的 ID 上。
      if (
        typeof window !== "undefined" &&
        window.location.pathname.replace(/\/$/, "").endsWith("/chat/" + session.id)
      ) {
        router.replace(`/agents/${encodeURIComponent(agentId)}/chat/`);
      }
      onChanged();
    }
  };

  // 触发器样式：SidebarMenuAction（用于扁平对话列表）接入
  // group-hover/menu-item；项目子行使用不同的 group 选择器
  //（group/menu-sub-item）和更小的芯片以适应 h-7 子按钮。
  // 两种触发器变体在此定义，调用者无需了解两种布局的细节。
  //
  // 子项变体使用 right:-20px，使芯片从 SidebarMenuSub 的
  // mx-3.5（14px 边距）加 px-2.5（10px 内边距）中突出，
  // 最终与父项目行的"…"操作按钮（位于 right:4px）对齐。
  // 14 + 10 - 4 = 20 → right:-20px。没有此偏移子芯片会比
  // 父芯片偏左约 20px。
  const triggerClass =
    variant === "menu-sub-item"
      ? "absolute top-1 right-[-20px] flex h-5 w-5 items-center justify-center rounded-md text-sidebar-foreground outline-hidden transition-opacity hover:bg-sidebar-accent hover:text-sidebar-accent-foreground aria-expanded:opacity-100 md:opacity-0 group-hover/menu-sub-item:opacity-100 group-focus-within/menu-sub-item:opacity-100 [&>svg]:size-4"
      : "absolute top-1.5 right-1 flex aspect-square w-5 items-center justify-center rounded-md p-0 text-sidebar-foreground outline-hidden transition-transform hover:bg-sidebar-accent hover:text-sidebar-accent-foreground aria-expanded:opacity-100 md:opacity-0 group-hover/menu-item:opacity-100 group-focus-within/menu-item:opacity-100 [&>svg]:size-4";

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <button type="button" className={triggerClass}>
              <MoreHorizontalIcon />
              <span className="sr-only">对话操作</span>
            </button>
          }
        />
        <DropdownMenuContent
          className="w-40 rounded-lg"
          side={isMobile ? "bottom" : "right"}
          align={isMobile ? "end" : "start"}
        >
          <DropdownMenuItem onClick={() => setEditOpen(true)}>
            <PencilIcon className="text-muted-foreground" />
            <span>编辑</span>
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            onClick={() => setDeleteOpen(true)}
            className="text-destructive focus:text-destructive"
          >
            <Trash2Icon className="text-destructive" />
            <span>删除</span>
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <EditTitleDialog
        open={editOpen}
        onOpenChange={setEditOpen}
        agentId={agentId}
        session={session}
        onSaved={onChanged}
      />

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除对话</AlertDialogTitle>
            <AlertDialogDescription>
              删除 <strong>{session.title || session.id}</strong>？此对话的全部消息历史将被删除且无法恢复。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction
              onClick={onConfirmDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function EditTitleDialog({
  open,
  onOpenChange,
  agentId,
  session,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  session: ChatRowSession;
  onSaved: () => void;
}) {
  const [draft, setDraft] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  // 每次对话框打开时重新填充草稿。否则编辑后取消再重新打开
  // 的用户会看到过期的草稿。
  React.useEffect(() => {
    if (open) setDraft(session.title ?? "");
  }, [open, session.title]);

  const save = async () => {
    const next = draft.trim();
    if (!next || next === session.title) {
      onOpenChange(false);
      return;
    }
    setSaving(true);
    try {
      await renameChatSession(agentId, session.id, next);
      onSaved();
    } finally {
      setSaving(false);
      onOpenChange(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>编辑对话标题</DialogTitle>
          <DialogDescription>
            重命名此对话，以便在侧边栏中查找。
          </DialogDescription>
        </DialogHeader>
        <Input
          autoFocus
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            // CJK 输入法合成活跃时忽略 Enter 键 ——
            // 否则选择候选词会过早提交对话框。
            if (e.nativeEvent.isComposing || e.keyCode === 229) return;
            if (e.key === "Enter") {
              e.preventDefault();
              save();
            }
          }}
          placeholder="对话标题"
        />
        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={saving}
          >
            取消
          </Button>
          <Button onClick={save} disabled={saving || !draft.trim()}>
            {saving ? "正在保存…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
