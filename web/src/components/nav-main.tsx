"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import {
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import type { LucideIcon } from "lucide-react";

export interface NavItem {
  title: string;
  // url 是导航目标。当提供 onClick 时可选 ——
  // 纯点击项（例如打开对话框的项）没有目标。
  url?: string;
  icon: LucideIcon;
  // active 覆盖默认的基于路径名前缀匹配。当两个项共享相同路径名、
  // 仅在查询参数不同时使用（例如"新建对话"和 /agents/<id>/chat/ 下
  // 的已打开会话，前缀规则会高亮两者）。
  active?: boolean;
  // onClick 存在时替换默认的 router.push。用于打开对话框而非导航的项。
  onClick?: () => void;
}

function isActive(pathname: string, href: string) {
  const norm = (s: string) => s.replace(/\/$/, "");
  return norm(pathname) === norm(href) || norm(pathname).startsWith(norm(href) + "/");
}

// label 可选 — 省略时跳过 SidebarGroupLabel 行，使该部分作为
// 无标签集群混入（用于独立的 Overview 链接和底部的设置条目）。
export function NavMain({
  label,
  items,
}: {
  label?: string;
  items: NavItem[];
}) {
  const pathname = usePathname();
  const router = useRouter();

  // 空闲时预取目标路由，使软导航在用户点击时已就绪 ——
  // 镜像了 <Link> 自动执行的操作，但我们选择不使用 Link
  // 以保证客户端导航。纯点击项（无 url）无需预取。
  React.useEffect(() => {
    items.forEach((item) => {
      if (item.url) router.prefetch(item.url);
    });
  }, [items, router]);

  // Base UI 的 SidebarMenuButton `render` 属性通过 React.cloneElement 合并，
  // 偶尔会丢失 Next <Link> 的内部点击处理器（每次点击变成完整
  // 页面刷新 → 可见侧边栏闪烁）。使用普通 <button> + 编程式
  // router.push 保证了客户端过渡。
  return (
    <SidebarGroup>
      {label && <SidebarGroupLabel>{label}</SidebarGroupLabel>}
      <SidebarMenu>
        {items.map((item) => {
          const active =
            item.active ?? (item.url ? isActive(pathname, item.url) : false);
          const handleClick = item.onClick
            ? item.onClick
            : item.url
              ? () => router.push(item.url!)
              : undefined;
          return (
            <SidebarMenuItem key={item.url ?? item.title}>
              <SidebarMenuButton
                isActive={active}
                tooltip={item.title}
                onClick={handleClick}
                onMouseEnter={() => {
                  if (item.url) router.prefetch(item.url);
                }}
              >
                <item.icon />
                <span>{item.title}</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          );
        })}
      </SidebarMenu>
    </SidebarGroup>
  );
}

// 导出给需要带 Next 客户端导航的真实锚点、但不想要侧边栏按钮样式的页面使用。
export { Link as NavLink };
