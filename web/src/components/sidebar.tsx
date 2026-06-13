"use client";

import * as React from "react";
import { useSearchParams } from "next/navigation";
import { AppSidebar } from "@/components/app-sidebar";
import {
  SidebarInset,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar";
// 页面头部插槽：页面调用 `usePageHeader(<jsx/>)` 在全局粘性头部的
// 侧边栏触发器右侧渲染内容。页面卸载时插槽清空。聊天使用此功能
// 在侧边栏切换按钮旁显示可编辑的会话标题。
interface PageHeaderContextValue {
  setNode: (node: React.ReactNode) => void;
}
const PageHeaderContext = React.createContext<PageHeaderContextValue | null>(
  null,
);

export function usePageHeader(node: React.ReactNode, deps: React.DependencyList = []) {
  const ctx = React.useContext(PageHeaderContext);
  React.useEffect(() => {
    if (!ctx) return;
    ctx.setNode(node);
    return () => ctx.setNode(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}

export function SidebarLayout({ children }: { children: React.ReactNode }) {
  const [headerNode, setHeaderNode] = React.useState<React.ReactNode>(null);
  const searchParams = useSearchParams();
  // `?actAs=<uid>` 表示管理员 / 智能体所有者正在只读查看另一个
  // 用户的聊天。平台侧边栏属于查看者的会话而非被模拟用户 —— 隐藏它
  //（及其折叠切换）保持界面专注于被检查的对话。
  const isActAsView = !!searchParams?.get("actAs");

  const headerCtx = React.useMemo<PageHeaderContextValue>(
    () => ({ setNode: setHeaderNode }),
    [],
  );

  if (isActAsView) {
    return (
      <PageHeaderContext.Provider value={headerCtx}>
        <div className="flex min-h-svh flex-col">
          <header className="sticky top-0 z-20 flex h-12 items-center gap-2 bg-background/80 px-3 backdrop-blur">
            {headerNode}
          </header>
          <div className="flex-1">{children}</div>
        </div>
      </PageHeaderContext.Provider>
    );
  }

  return (
    <PageHeaderContext.Provider value={headerCtx}>
      <SidebarProvider>
        <AppSidebar />
        <SidebarInset>
          <header className="sticky top-0 z-20 flex h-12 items-center gap-2 bg-background/80 px-3 backdrop-blur">
            <SidebarTrigger className="-ml-1" />
            {headerNode}
          </header>
          <div className="flex-1">{children}</div>
        </SidebarInset>
      </SidebarProvider>
    </PageHeaderContext.Provider>
  );
}
