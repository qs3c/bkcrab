"use client";

import * as React from "react";
import { usePathname, useSearchParams } from "next/navigation";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from "@/components/ui/sidebar";
import { AgentSwitcher, AgentSwitcherItem } from "@/components/team-switcher";
import { NavMain, NavItem } from "@/components/nav-main";
import { NavSessions, SessionItem } from "@/components/nav-projects";
import { NavProjectsList } from "@/components/nav-projects-list";
import { NavUser } from "@/components/nav-user";
import { AgentSettingsDialog } from "@/components/agent-settings-dialog";
import {
  BotIcon,
  BrainIcon,
  CoinsIcon,
  KeyRoundIcon,
  LayoutDashboardIcon,
  MessagesSquareIcon,
  PlusIcon,
  SettingsIcon,
  SparklesIcon,
  UsersIcon,
  WrenchIcon,
} from "lucide-react";
import {
  getAgent,
  getAgents,
  getChatSessions,
  getMe,
  getStatus,
  listProjects,
  type MeResponse,
  type ProjectEntry,
  type StatusResponse,
} from "@/lib/api";

// 从类似 /agents/default/chat/ 的路径中提取智能体 ID。第二个
// 捕获组是子路由的显式白名单，这样裸 /agents/
// 首页会保持平台导航而不是切换到智能体导航。
// 新增智能体范围的路由时应在此处添加 — `project` 就曾
// 在项目聊天路由引入时被遗漏，导致侧边栏在 /agents/<id>/project/...
// 仍然显示平台导航
function extractAgentId(pathname: string): string | null {
  const match = pathname.match(
    /^\/agents\/([^/]+)\/(chat|customize|skills|models|sessions|channels|chats|scheduler|project)/,
  );
  return match ? match[1] : null;
}

// 侧边栏导航渲染为一系列带标签的分组，用户可以按领域浏览，
// 而不是一个扁平列表：
//
//   (无标签)    概览                                   — 登录仪表盘
//   Agent       智能体 · 模型 · 技能 · 工具              — 智能体构建界面
//   用户        用户 · 对话 · 令牌用量 · API 密钥         — 管理员工具
//   (无标签)    设置                                    — 打开用户对话框
//
// 技能/工具以及用户/对话/令牌用量等管理入口仅管理员可见。
// 非管理员只能看到包含"智能体"和"模型"的 Agent 分组，
// 以及仅含 API 密钥的精简"用户"分组。设置项仅响应点击 —
// 其 onClick 在渲染时绑定，以便调用组件状态。
const OVERVIEW_ITEM: NavItem = {
  title: "概览",
  url: "/overview/",
  icon: LayoutDashboardIcon,
};

const USER_AGENT_GROUP: NavItem[] = [
  { title: "智能体", url: "/agents/", icon: BotIcon },
  { title: "模型", url: "/models/", icon: BrainIcon },
];

const ADMIN_AGENT_GROUP: NavItem[] = [
  { title: "智能体", url: "/agents/", icon: BotIcon },
  { title: "模型", url: "/models/", icon: BrainIcon },
  { title: "技能", url: "/skills/", icon: SparklesIcon },
  { title: "工具", url: "/tools/", icon: WrenchIcon },
];

const USER_USER_GROUP: NavItem[] = [
  { title: "API 密钥", url: "/apikeys/", icon: KeyRoundIcon },
];

const ADMIN_USER_GROUP: NavItem[] = [
  { title: "用户", url: "/admin/users/", icon: UsersIcon },
  { title: "对话", url: "/admin/chats/", icon: MessagesSquareIcon },
  { title: "令牌用量", url: "/admin/usage/", icon: CoinsIcon },
  { title: "API 密钥", url: "/apikeys/", icon: KeyRoundIcon },
];

// "新建对话"仅在我们停留在 /chat/ 页面且没有打开会话时
// 处于激活状态。会话可以两种方式编码：
//   - `/chat/` 上的 `?session=<id>` 查询参数
//   - 路径段：`/chat/<sessionId>/`
// 两种形式都表示打开了特定会话，因此"新建对话"条目不
// 应高亮。我们检查精确路径名（而非 startsWith），
// 以使路径段形式不会误判。
//
// 配置标签页（自定义 / 模型 / 技能 / 渠道 /
// 调度器）位于底部设置对话框中 — 仅限所有者 —
// 因此侧边栏导航本身只展示"新建对话"，无论角色如何。
const AGENT_NAV = (
  agentId: string,
  pathname: string,
  hasSession: boolean,
): NavItem[] => {
  const base = `/agents/${agentId}/chat`;
  const onNewChatRoute = pathname === base || pathname === `${base}/`;
  return [
    {
      title: "新建对话",
      url: `${base}/`,
      icon: PlusIcon,
      active: onNewChatRoute && !hasSession,
    },
  ];
};

export function AppSidebar(props: React.ComponentProps<typeof Sidebar>) {
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const activeAgentId = extractAgentId(pathname);
  const hasOpenSession = !!searchParams?.get("session");

  const [status, setStatus] = React.useState<StatusResponse | null>(null);
  const [me, setMe] = React.useState<MeResponse | null>(null);
  const [agents, setAgents] = React.useState<AgentSwitcherItem[]>([]);
  // 每个智能体的角色标志 — 所有者 vs 查看者（只读，
  // 由其他用户共享）。驱动 AGENT_NAV 是否展示配置标签页。
  const [agentRoles, setAgentRoles] = React.useState<Record<string, "owner" | "viewer">>({});
  const [sessions, setSessions] = React.useState<SessionItem[]>([]);
  const [projects, setProjects] = React.useState<ProjectEntry[]>([]);
  // 单个对话框状态覆盖两个入口：智能体范围的
  // 底部按钮（完整 Agent + 用户标签页）和平台导航的
  // 设置项（仅用户标签页）。settingsUserOnly 切换模式。
  const [settingsOpen, setSettingsOpen] = React.useState(false);
  const [settingsUserOnly, setSettingsUserOnly] = React.useState(false);

  // 保持状态轮询，使在线指示和管理员标志保持最新。
  React.useEffect(() => {
    getStatus().then(setStatus).catch(() => {});
    const iv = setInterval(() => {
      getStatus().then(setStatus).catch(() => {});
    }, 15000);
    return () => clearInterval(iv);
  }, []);

  // 获取当前用户一次，以便底部栏显示其名称和角色。
  React.useEffect(() => {
    getMe().then(setMe).catch(() => {});
  }, []);

  // 智能体列表驱动侧边栏顶部的切换下拉框。
  React.useEffect(() => {
    getAgents()
      .then((list) => {
        setAgents(list.map((a) => ({ id: a.id, name: a.name, model: a.model })));
        const roles: Record<string, "owner" | "viewer"> = {};
        for (const a of list) {
          roles[a.id] = a.role === "viewer" ? "viewer" : "owner";
        }
        setAgentRoles(roles);
      })
      .catch(() => {});
  }, []);

  // 当活跃智能体不在调用者的自有列表中时 — 例如
  // 超级管理员与其他用户的智能体对话 — 单独获取其名称
  // 并插入，以便切换器标题显示真实名称而非回退到"BkClaw"。
  // 单智能体端点也返回角色，因此在此一并捕获。
  React.useEffect(() => {
    if (!activeAgentId) return;
    if (agents.some((a) => a.id === activeAgentId)) return;
    let aborted = false;
    getAgent(activeAgentId)
      .then((a) => {
        if (aborted || !a) return;
        setAgents((prev) =>
          prev.some((x) => x.id === a.id)
            ? prev
            : [...prev, { id: a.id, name: a.name, model: a.model }],
        );
        if (a.role === "viewer" || a.role === "owner") {
          setAgentRoles((prev) => ({ ...prev, [a.id]: a.role as "owner" | "viewer" }));
        }
      })
      .catch(() => {});
    return () => {
      aborted = true;
    };
  }, [activeAgentId, agents]);

  // 会话和项目仅在选择特定智能体时才有意义。
  // 当活跃智能体变更或聊天页面广播
  // `bkclaw:sessions-changed` 事件时（例如重命名 /
  // 新建对话 / 创建项目后），我们会重新获取两者，
  // 以便侧边栏无需刷新页面即可保持同步。项目与
  // 会话一起获取，因为项目内创建聊天也会影响其下的会话列表。
  React.useEffect(() => {
    if (!activeAgentId) {
      setSessions([]);
      setProjects([]);
      return;
    }
    const refetch = () => {
      getChatSessions(activeAgentId)
        .then((list) =>
          setSessions(
            list.map((s) => ({
              id: s.id,
              title: s.title || s.preview || s.id,
              thumbnailUrl: s.thumbnailUrl,
              channel: s.channel,
              projectId: s.projectId,
            })),
          ),
        )
        .catch(() => {});
      listProjects(activeAgentId)
        .then(setProjects)
        .catch(() => {});
    };
    refetch();
    const onChange = (e: Event) => {
      const detail = (e as CustomEvent<{ agentId?: string }>).detail;
      if (!detail || !detail.agentId || detail.agentId === activeAgentId) {
        refetch();
      }
    };
    window.addEventListener("bkclaw:sessions-changed", onChange);
    return () => {
      window.removeEventListener("bkclaw:sessions-changed", onChange);
    };
  }, [activeAgentId]);

  // broadcastSessionsChanged 触发 NavSessions 监听的
  // 同一自定义事件，因此项目变更会同时刷新项目列表
  // 和会话列表（项目中的新聊天会出现在其项目下，
  // 而项目的会话计数驱动删除阻止逻辑）。
  const broadcastSessionsChanged = React.useCallback(() => {
    if (typeof window !== "undefined" && activeAgentId) {
      window.dispatchEvent(
        new CustomEvent("bkclaw:sessions-changed", {
          detail: { agentId: activeAgentId },
        }),
      );
    }
  }, [activeAgentId]);

  const isAdmin = status?.isAdmin ?? false;
  // quotaLocked = 调用者的 agent_quota=0（仅管理员分配，
  // 典型的单智能体客户模式）。智能体切换器标题被锁定
  // （静态标签，无"管理智能体"下拉），但 /agents 页面本身
  // 仍然可达，以便他们浏览已分配的智能体并进入聊天 —
  // 只是隐藏了创建按钮。因此我们保持"智能体"导航条目可见。
  const quotaLocked = me?.user?.agentQuota === 0;

  return (
    <Sidebar collapsible="icon" {...props}>
      <SidebarHeader>
        <AgentSwitcher
          agents={agents}
          activeAgentId={activeAgentId}
          locked={
            quotaLocked ||
            (!!activeAgentId && agentRoles[activeAgentId] === "viewer")
          }
        />
      </SidebarHeader>
      <SidebarContent>
        {activeAgentId ? (
          <NavMain
            label="Agent"
            items={AGENT_NAV(activeAgentId, pathname, hasOpenSession)}
          />
        ) : (
          <>
            <NavMain items={[OVERVIEW_ITEM]} />
            <NavMain
              label="Agent"
              items={isAdmin ? ADMIN_AGENT_GROUP : USER_AGENT_GROUP}
            />
            <NavMain
              label="用户"
              items={isAdmin ? ADMIN_USER_GROUP : USER_USER_GROUP}
            />
          </>
        )}
        {/* 项目按（用户，智能体）划分，因此共享智能体上的查看者
            看到/创建的是自己的项目 — 所有者的项目保持
            私有。下面的仅所有者设置对话框不受影响：
            项目 CRUD 对打开智能体的人可读写，但
            智能体配置（技能、渠道、模型）仍归所有者。 */}
        {activeAgentId && (
          <NavProjectsList
            agentId={activeAgentId}
            projects={projects}
            sessions={sessions}
            onChanged={broadcastSessionsChanged}
          />
        )}
        <NavSessions agentId={activeAgentId} sessions={sessions} />
      </SidebarContent>
      <SidebarFooter>
        {/* 设置固定在底部栏，无论当前路由如何，入口位置不变。
            模式取决于 activeAgentId：在智能体路由上，对话框以
            完整智能体标签页打开（个人资料 / 自定义 / 模型 / 技能 /
            渠道 / 调度器）— 查看者看到过滤后的子集；在平台路由上以
            仅用户模式打开（账户 / 通用）。 */}
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              tooltip="Settings"
              onClick={() => {
                setSettingsUserOnly(!activeAgentId);
                setSettingsOpen(true);
              }}
            >
              <SettingsIcon />
              <span>设置</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
        <NavUser
          name={
            me?.user?.displayName ||
            me?.user?.username ||
            (isAdmin ? "管理员" : "用户")
          }
          subtitle={
            me?.user?.role === "super_admin"
              ? "超级管理员"
              : me?.user?.role === "user"
                ? "用户"
                : isAdmin
                  ? "超级管理员"
                  : "用户"
          }
        />
      </SidebarFooter>
      <SidebarRail />
      <AgentSettingsDialog
        open={settingsOpen}
        onOpenChange={setSettingsOpen}
        userOnly={settingsUserOnly}
        role={
          activeAgentId && agentRoles[activeAgentId] === "viewer"
            ? "viewer"
            : "owner"
        }
        isAdmin={isAdmin}
      />
    </Sidebar>
  );
}
