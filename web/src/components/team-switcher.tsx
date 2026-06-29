"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar";
import { Bot, ChevronsUpDownIcon, PlusIcon } from "lucide-react";

// AgentAvatar 在可用时显示智能体上传的 /api/agents/{id}/files/avatar.png，
// 无智能体时回退为 BkCrab 标志（平台头部），智能体未上传自定义头像时
//（图片 404）回退为 Bot 图标。
function AgentAvatar({
  agentId,
  size = 32,
}: {
  agentId?: string | null;
  size?: number;
}) {
  const [failed, setFailed] = React.useState(false);
  React.useEffect(() => {
    setFailed(false);
  }, [agentId]);

  if (!agentId) {
    return (
      <img
        src="/logo.png"
        alt="BkCrab"
        width={size}
        height={size}
        className="shrink-0 rounded-lg object-contain"
        style={{ width: size, height: size }}
      />
    );
  }
  if (failed) {
    return (
      <div
        className="flex shrink-0 items-center justify-center rounded-lg bg-primary/10 dark:bg-primary/15 border border-primary/15"
        style={{ width: size, height: size }}
      >
        <Bot className="text-primary" style={{ width: size * 0.55, height: size * 0.55 }} />
      </div>
    );
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={`/api/agents/${agentId}/files/avatar.png`}
      alt=""
      width={size}
      height={size}
      className="shrink-0 rounded-lg object-cover"
      style={{ width: size, height: size }}
      onError={() => setFailed(true)}
    />
  );
}

export interface AgentSwitcherItem {
  id: string;
  name?: string;
  model?: string;
}

// AgentSwitcher 渲染侧边栏头部。
  //
  //   activeAgentId 已设置   → 显示该智能体的显示名称和 ID，下拉列表
  //                           列出每个智能体供快速切换
  //   activeAgentId 未设置   → 显示"BkCrab"（平台品牌）。下拉列表
  //                           仍然列出智能体，以便用户从任何非智能体页面跳入。
  //
  // 我们从不会自动将第一个智能体提升到头部 —— 管理页面（智能体列表、
  // API 密钥、设置……）的头部保持中性。
export function AgentSwitcher({
  agents,
  activeAgentId,
  onSelect,
  locked = false,
}: {
  agents: AgentSwitcherItem[];
  activeAgentId?: string | null;
  onSelect?: (id: string) => void;
  // locked 隐藏下拉触发器 / 智能体列表 / "管理智能体" ——
  // 头部变成静态标签 + 头像。用于调用者不是活跃智能体所有者的情况
  //（公开链接访客 / super_admin 查看他人智能体），使其不会看到
  // 实际上不属于他们的智能体列表。
  locked?: boolean;
}) {
  const { isMobile } = useSidebar();
  const router = useRouter();

  const active = activeAgentId
    ? agents.find((a) => a.id === activeAgentId) ?? null
    : null;

  const goto = React.useCallback(
    (id: string) => {
      if (onSelect) onSelect(id);
      else router.push(`/agents/${id}/chat/`);
    },
    [onSelect, router],
  );

  const headerLabel = active ? active.name || active.id : "BkCrab";

  if (locked) {
    return (
      <SidebarMenu>
        <SidebarMenuItem>
          <SidebarMenuButton size="lg" className="cursor-default">
            <AgentAvatar agentId={active?.id} size={32} />
            <div className="grid flex-1 text-left text-sm leading-tight">
              <span className="truncate font-medium">{headerLabel}</span>
            </div>
          </SidebarMenuButton>
        </SidebarMenuItem>
      </SidebarMenu>
    );
  }

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <SidebarMenuButton
                size="lg"
                className="data-open:bg-sidebar-accent data-open:text-sidebar-accent-foreground"
              />
            }
          >
            <AgentAvatar agentId={active?.id} size={32} />
            <div className="grid flex-1 text-left text-sm leading-tight">
              <span className="truncate font-medium">{headerLabel}</span>
            </div>
            <ChevronsUpDownIcon className="ml-auto" />
          </DropdownMenuTrigger>
          <DropdownMenuContent
            className="min-w-56 rounded-lg"
            align="start"
            side={isMobile ? "bottom" : "right"}
            sideOffset={4}
          >
            {agents.length > 0 && (
              <>
                <DropdownMenuGroup>
                  <DropdownMenuLabel className="text-xs text-muted-foreground">
                    智能体
                  </DropdownMenuLabel>
                  {agents.map((a) => (
                    <DropdownMenuItem
                      key={a.id}
                      onClick={() => goto(a.id)}
                      className="gap-2 p-2"
                    >
                      <AgentAvatar agentId={a.id} size={24} />
                      <span className="flex-1 truncate">{a.name || a.id}</span>
                    </DropdownMenuItem>
                  ))}
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
              </>
            )}
            <DropdownMenuGroup>
              <DropdownMenuItem
                className="gap-2 p-2"
                onClick={() => router.push("/agents/")}
              >
                <div className="flex size-6 items-center justify-center rounded-md border bg-transparent">
                  <PlusIcon className="size-4" />
                </div>
                <div className="font-medium text-muted-foreground">
                  管理智能体
                </div>
              </DropdownMenuItem>
            </DropdownMenuGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  );
}
