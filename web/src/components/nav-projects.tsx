"use client";

import * as React from "react";
import { usePathname, useRouter } from "next/navigation";
import {
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { CheckCircle2, LoaderCircle, MoreHorizontal } from "lucide-react";
import { moveChatSessionToProject } from "@/lib/api";
import { ChannelIcon, channelLabel } from "@/components/channel-icon";
import { ChatRowActions } from "@/components/chat-row-actions";

// 聊天会话拖拽时在 dataTransfer 中携带的 MIME 类型。自定义类型，
// 避免响应无关拖放（从应用外拖入文本、桌面文件等）。
export const CHAT_DRAG_MIME = "application/x-bkcrab-chat";

// 限制侧边栏列表长度，避免一个活跃智能体把其他导航项推出屏幕。
// 完整列表在 /agents/<id>/chats，带分页 —— 末尾的"更多…"行链接过去。
const MAX_SIDEBAR_SESSIONS = 10;

export interface SessionItem {
  id: string;
  title: string;
  // 会话首条用户消息包含图片附件时设置。
  // 渲染为标题前的小缩略图，使多模态对话显示"图片 + 文本"
  // 而不是仅仅文本标签。
  thumbnailUrl?: string;
  // channel 驱动各渠道图标前缀（telegram / wechat /
  // line / web 等）。为空时回退到 web 图标。
  channel?: string;
  // projectId 设置后标记此对话属于某个项目 —— NavSessions
  // 过滤掉这些对话，使它们仅嵌套显示在项目下方
  //（NavProjectsList 渲染它们）。
  projectId?: string;
  lastTurnStatus?: "running" | "done" | string;
}

export function NavSessions({
  agentId,
  sessions,
}: {
  agentId: string | null;
  sessions: SessionItem[];
}) {
  const pathname = usePathname();
  const router = useRouter();
  // "将项目对话拖回对话组"的放置区状态。
  // 整个组作为目标 —— 仅在拖拽数据携带 CHAT_DRAG_MIME 载荷且源对话
  // 当前位于某个项目内时才高亮（将已松散的对话拖放到其自身组是空操作，
  // 抑制高亮使这一点自明）。此 hook 必须在下方的 early-return 之前运行，
  // 以保持各次渲染间 hook 调用顺序稳定。
  const [chatsDropActive, setChatsDropActive] = React.useState(false);

  // 对同一聊天行去重快速双击 —— 参见 nav-projects-list.tsx 中
  // 对应的块，了解它防止的连接池饥饿问题。hook 必须在
  // early-return 之前运行。
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

  // 任何变更（重命名 / 删除）都会广播，以便 AppSidebar 重新拉取，
  // 同时聊天页面（如果打开）也重新同步其本地会话列表。
  const broadcastChange = () => {
    if (typeof window !== "undefined") {
      window.dispatchEvent(
        new CustomEvent("bkcrab:sessions-changed", {
          detail: { agentId },
        }),
      );
    }
  };

  const onChatsDragOver = (e: React.DragEvent) => {
    if (!hasChatPayload(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
    if (!chatsDropActive) setChatsDropActive(true);
  };
  const onChatsDragLeave = () => setChatsDropActive(false);
  const onChatsDrop = async (e: React.DragEvent) => {
    if (!hasChatPayload(e)) return;
    e.preventDefault();
    setChatsDropActive(false);
    const sid = e.dataTransfer.getData(CHAT_DRAG_MIME);
    if (!sid) return;
    const sess = sessions.find((s) => s.id === sid);
    // 已经是松散的 —— 无需操作。
    if (!sess || !sess.projectId) return;
    const res = await moveChatSessionToProject(agentId, sid, "");
    if (res?.error) {
      // 内联显示失败；侧边栏暂无 toast 基础设施，
      // 因此 console 错误 + 弹窗可防止用户静默丢失操作。
      console.error("move chat to loose failed:", res.error);
      window.alert(`移动会话失败：${res.error}`);
      return;
    }
    broadcastChange();
  };

  return (
    <>
      <SidebarGroup
        className="group-data-[collapsible=icon]:hidden"
        onDragOver={onChatsDragOver}
        onDragLeave={onChatsDragLeave}
        onDrop={onChatsDrop}
      >
        <SidebarGroupLabel>对话</SidebarGroupLabel>
        <SidebarMenu
          className={
            chatsDropActive
              ? "rounded-md outline outline-2 outline-primary/40"
              : ""
          }
        >
          {/* 跳过属于项目的对话 —— 它们在 NavProjectsList 中
              嵌套渲染在其项目下，因此扁平的"对话"部分
              仅显示非项目对话。 */}
          {sessions.filter((s) => !s.projectId).slice(0, MAX_SIDEBAR_SESSIONS).map((s) => {
            const href = `${chatBase}${encodeURIComponent(s.id)}/`;
            // 路径格式：/agents/<aid>/chat/<sid>/。精确匹配，使兄弟对话不会
            // 仅因路径名共享聊天基础路径而高亮。
            const active = pathname === href || pathname === href.replace(/\/$/, "");
            return (
              <SessionRow
                key={s.id}
                agentId={agentId}
                session={s}
                active={active}
                onOpen={() => navigateOnce(href)}
                onChanged={broadcastChange}
              />
            );
          })}
          {sessions.length > MAX_SIDEBAR_SESSIONS && (
            <SidebarMenuItem>
              <SidebarMenuButton
                onClick={() => navigateOnce(`/agents/${agentId}/chats`)}
                tooltip="See all chats"
                className="text-muted-foreground"
              >
                <MoreHorizontal className="size-4" />
                <span>更多</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          )}
          {sessions.length === 0 && (
            <SidebarMenuItem>
              <div className="px-2 py-1.5 text-xs text-muted-foreground">
                暂无对话
              </div>
            </SidebarMenuItem>
          )}
        </SidebarMenu>
      </SidebarGroup>
    </>
  );
}

// hasChatPayload：廉价的判断谓词，放置目标用于门控 preventDefault + 高亮。
// dataTransfer.types 在 drag enter/over 期间可用，因此我们可以避免为
// 偶然穿越侧边栏的不相关拖放（text/uri-list、Files 等）亮起。
export function hasChatPayload(e: React.DragEvent): boolean {
  return Array.from(e.dataTransfer.types).includes(CHAT_DRAG_MIME);
}

function SessionRow({
  agentId,
  session,
  active,
  onOpen,
  onChanged,
}: {
  agentId: string;
  session: SessionItem;
  active: boolean;
  onOpen: () => void;
  onChanged: () => void;
}) {
  const onDragStart = (e: React.DragEvent) => {
    e.dataTransfer.setData(CHAT_DRAG_MIME, session.id);
    e.dataTransfer.effectAllowed = "move";
  };
  return (
    <SidebarMenuItem draggable onDragStart={onDragStart}>
      <SidebarMenuButton
        isActive={active}
        tooltip={`${channelLabel(session.channel)} · ${session.title}`}
        onClick={onOpen}
      >
        {session.thumbnailUrl ? (
          // eslint-disable-next-line @next/next/no-img-element
          <img
            src={session.thumbnailUrl}
            alt=""
            className="h-5 w-5 shrink-0 rounded object-cover"
          />
        ) : (
          <ChannelIcon channel={session.channel} />
        )}
        <span className="flex min-w-0 flex-1 items-center gap-1.5">
          <SessionStatusIndicator status={session.lastTurnStatus} />
          <span className="truncate">{session.title || session.id}</span>
        </span>
      </SidebarMenuButton>
      <ChatRowActions
        agentId={agentId}
        session={{ id: session.id, title: session.title }}
        onChanged={onChanged}
      />
    </SidebarMenuItem>
  );
}

export function SessionStatusIndicator({
  status,
}: {
  status?: SessionItem["lastTurnStatus"];
}) {
  if (status === "running") {
    return (
      <span
        className="inline-flex size-3.5 shrink-0 items-center justify-center text-amber-500"
        title="运行中"
        aria-label="运行中"
      >
        <LoaderCircle className="size-3.5 animate-spin" />
        <span className="sr-only">运行中</span>
      </span>
    );
  }
  if (status === "done") {
    return (
      <span
        className="inline-flex size-3.5 shrink-0 items-center justify-center text-emerald-600 dark:text-emerald-400"
        title="已完成"
        aria-label="已完成"
      >
        <CheckCircle2 className="size-3.5" />
        <span className="sr-only">已完成</span>
      </span>
    );
  }
  return null;
}
