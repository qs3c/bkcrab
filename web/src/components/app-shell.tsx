"use client";

import * as React from "react";
import { usePathname } from "next/navigation";
import { SidebarLayout } from "@/components/sidebar";

// 自行渲染的路径（不带侧边栏框架）。/signup 在此处是因为
// 未登录时直接访问它（例如通过管理员邀请链接）会泄漏已认证
// 应用框架——概览 / 智能体 / 模型侧边栏——给尚未注册的访客。
const BARE_PATHS = ["/", "/onboard", "/signup"];

function wantsSidebar(pathname: string) {
  if (BARE_PATHS.includes(pathname)) return false;
  if (pathname.startsWith("/onboard/")) return false;
  if (pathname.startsWith("/signup/")) return false;
  return true;
}

// AppShell 为每个已认证页面挂载一次 SidebarLayout，并在客户端
// 导航期间保持该实例存活。此前每个路由段都有自己的 layout.tsx
// 包裹 SidebarLayout，导致 Next 在每次顶级导航时卸载再重新挂载
// 侧边栏——触发一次新的 status / agents / sessions 请求和可见闪烁。
// 根层只有一个 shell 意味着侧边栏及其副作用在导航间持久存在。
export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  if (!wantsSidebar(pathname)) {
    return <>{children}</>;
  }
  return <SidebarLayout>{children}</SidebarLayout>;
}
