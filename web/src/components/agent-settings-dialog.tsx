"use client";

import * as React from "react";
import {
  BrainIcon,
  ClockIcon,
  CoinsIcon,
  IdCardIcon,
  InfoIcon,
  LayersIcon,
  Palette,
  Plug,
  RadioIcon,
  SparklesIcon,
  UserCog,
  Wand2Icon,
} from "lucide-react";

import { Dialog, DialogContent } from "@/components/ui/dialog";
import { cn } from "@/lib/utils";

import AgentProfilePanel from "@/components/agent-profile-panel";
import AgentCustomizePage from "@/app/agents/[id]/customize/page";
import AgentModelsPage from "@/app/agents/[id]/models/page";
import AgentContextPage from "@/app/agents/[id]/context/page";
import AgentSkillsPage from "@/app/agents/[id]/skills/page";
import AgentPluginsPage from "@/app/agents/[id]/plugins/page";
import AgentChannelsPage from "@/app/agents/[id]/channels/page";
import AgentSchedulerPage from "@/app/agents/[id]/scheduler/page";
import AgentUsagePage from "@/app/agents/[id]/usage/page";
import AccountSettingsPage from "@/app/settings/account/page";
import GeneralSettingsPage from "@/app/settings/general/page";
import UserModelsPage from "@/app/models/page";
import AboutSettingsPage from "@/app/settings/about/page";

export type AgentSettingsTab =
  | "profile"
  | "customize"
  | "models"
  | "context"
  | "skills"
  | "plugins"
  | "channels"
  | "scheduler"
  | "usage"
  | "account"
  | "general"
  | "about";

type TabIcon = React.ComponentType<{ className?: string }>;

const AGENT_TABS: Array<{ id: AgentSettingsTab; label: string; icon: TabIcon }> = [
  { id: "profile", label: "资料", icon: IdCardIcon },
  { id: "customize", label: "自定义", icon: Wand2Icon },
  { id: "models", label: "模型", icon: BrainIcon },
  { id: "context", label: "上下文", icon: LayersIcon },
  { id: "skills", label: "技能", icon: SparklesIcon },
  { id: "plugins", label: "插件", icon: Plug },
  { id: "channels", label: "渠道", icon: RadioIcon },
  { id: "scheduler", label: "定时任务", icon: ClockIcon },
  { id: "usage", label: "令牌用量", icon: CoinsIcon },
];

// Runtime 故意只放在独立的 /settings/runtime 页面（由 super_admin 门控）——
// 它是部署级别的开关，不是普通聊天者会在每个智能体对话框中需要的东西。
const USER_TABS: Array<{ id: AgentSettingsTab; label: string; icon: TabIcon }> = [
  { id: "account", label: "账户", icon: UserCog },
  { id: "general", label: "常规", icon: Palette },
  // About 展示网关版本号和升级提示 — 仅对运维人员（super_admin）有用，
  // 下面对普通用户过滤掉了。
  { id: "about", label: "关于", icon: InfoIcon },
];

// 标签式配置面板。同时承载每个智能体的页面（自定义 / 模型 / 技能 /
  // 渠道 / 定时任务）和每个用户的页面（账户 / 常规 / Runtime[仅管理员]），
  // 这样点击侧边栏的设置按钮就能覆盖用户可能想修改的一切。每个标签
  // 懒加载现有页面组件 — 切换标签会卸载上一个面板，这没问题，因为
  // 页面都是自含的并在挂载时重新拉取。
  //
  // role="viewer" 隐藏仅限所有者的智能体标签（资料、自定义、技能、
  // 定时任务、用量），仅暴露智能体下的模型 + 渠道 — 访问者可以为自己的
  // 共享智能体固定模型，绑定自己的 IM 账号，但不能触碰智能体的身份 /
  // 技能 / 定时任务。模型标签 ID 与所有者共享；下面的渲染分支为所有者
  // 选择智能体范围页面、为访问者选择用户范围页面（同一标签位，不同写入方）。
export function AgentSettingsDialog({
  open,
  onOpenChange,
  defaultTab,
  role = "owner",
  userOnly = false,
  isAdmin = false,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  defaultTab?: AgentSettingsTab;
  role?: "owner" | "viewer";
  // userOnly 隐藏整个智能体部分。用于平台侧边栏的设置按钮，
  // 该按钮没有智能体上下文 — 仅应暴露账户 + 常规。
  userOnly?: boolean;
  // isAdmin 门控 super_admin 专用标签（目前仅 About — 网关版本+
  // 升级提示属于运维信息，不是终端用户信息）。
  isAdmin?: boolean;
}) {
  const agentTabs = userOnly
    ? []
    : role === "viewer"
      ? AGENT_TABS.filter((t) => t.id === "models" || t.id === "channels")
      : AGENT_TABS;
  const userTabs = isAdmin ? USER_TABS : USER_TABS.filter((t) => t.id !== "about");
  // 选择初始标签：userOnly 打开常规（用户部分）；访问者落在模型
  // （其拥有的第一个智能体标签）；所有者落在资料。
  const initialTab: AgentSettingsTab =
    defaultTab ??
    (userOnly ? "general" : role === "viewer" ? "models" : "profile");
  const [tab, setTab] = React.useState<AgentSettingsTab>(initialTab);

  // 对话框重新打开时重置到请求的标签，确保再次点击侧边栏
  // 设置按钮时总是落在同一位置。
  React.useEffect(() => {
    if (open) setTab(initialTab);
  }, [open, initialTab]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className={cn(
          "p-0 gap-0 overflow-hidden",
          "h-[85vh] w-[95vw] max-w-[1100px] sm:max-w-[1100px]",
          "grid grid-cols-[220px_1fr] grid-rows-1",
        )}
      >
        <aside className="flex flex-col gap-1 border-r bg-muted/40 p-3 overflow-y-auto">
          {agentTabs.length > 0 && (
            <>
              <SectionLabel>智能体</SectionLabel>
              {agentTabs.map((t) => (
                <TabButton
                  key={t.id}
                  tab={t}
                  active={tab === t.id}
                  onSelect={setTab}
                />
              ))}
            </>
          )}
          <SectionLabel className={agentTabs.length > 0 ? "mt-3" : undefined}>
            用户
          </SectionLabel>
          {userTabs.map((t) => (
            <TabButton
              key={t.id}
              tab={t}
              active={tab === t.id}
              onSelect={setTab}
            />
          ))}
        </aside>
        <div className="overflow-y-auto">
          {tab === "profile" && <AgentProfilePanel />}
          {tab === "customize" && <AgentCustomizePage />}
          {tab === "models" &&
            (role === "viewer" ? <UserModelsPage /> : <AgentModelsPage />)}
          {tab === "context" && <AgentContextPage />}
          {tab === "skills" && <AgentSkillsPage />}
          {tab === "plugins" && <AgentPluginsPage />}
          {tab === "channels" && <AgentChannelsPage />}
          {tab === "scheduler" && <AgentSchedulerPage />}
          {tab === "usage" && <AgentUsagePage />}
          {tab === "account" && (
            <div className="p-6 max-w-3xl">
              <AccountSettingsPage />
            </div>
          )}
          {tab === "general" && (
            <div className="p-6 max-w-3xl">
              <GeneralSettingsPage />
            </div>
          )}
          {tab === "about" && (
            <div className="p-6 max-w-3xl">
              <AboutSettingsPage />
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}

function SectionLabel({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "px-2 pt-1 pb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground",
        className,
      )}
    >
      {children}
    </div>
  );
}

function TabButton({
  tab,
  active,
  onSelect,
}: {
  tab: { id: AgentSettingsTab; label: string; icon: TabIcon };
  active: boolean;
  onSelect: (id: AgentSettingsTab) => void;
}) {
  const Icon = tab.icon;
  return (
    <button
      type="button"
      onClick={() => onSelect(tab.id)}
      className={cn(
        "flex items-center gap-2 rounded-md px-2.5 py-2 text-sm text-left transition-colors",
        active
          ? "bg-accent text-accent-foreground font-medium"
          : "text-foreground/80 hover:bg-accent/50",
      )}
    >
      <Icon className="size-4 shrink-0" />
      <span>{tab.label}</span>
    </button>
  );
}
