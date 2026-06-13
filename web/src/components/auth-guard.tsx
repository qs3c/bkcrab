"use client";

import { useState, useEffect } from "react";
import { useRouter, usePathname } from "next/navigation";
import { getMe } from "@/lib/api";
import { LoginScreen } from "./login-screen";

interface AuthGuardProps {
  children: React.ReactNode;
}

// 需要 admin（super_admin）角色的路由。服务端 API 做了权威性验证；
  // 客户端门禁只是阻止非管理员进入会渲染空壳 / 403 页面的页面。
  //
  // /settings、/models 和 /apikeys 故意不在此列 ——
  // settings 隐藏了 Runtime；models 合并系统+用户并显示徽章；
  // apikeys 允许非管理员签发 type=user/agent（仅 type=admin
  // 需要 super_admin，该门控在创建处理器内部）。
const ADMIN_PATH_PREFIXES = [
  "/admin/",
  "/skills",
  "/providers",
  "/channels",
  "/channels-config",
  "/plugins",
  "/tools",
  "/cron",
];

function isAdminPath(pathname: string): boolean {
  return ADMIN_PATH_PREFIXES.some(
    (p) => pathname === p || pathname === p.replace(/\/$/, "") || pathname.startsWith(p + (p.endsWith("/") ? "" : "/")),
  );
}

export function AuthGuard({ children }: AuthGuardProps) {
  const router = useRouter();
  const pathname = usePathname();
  const [checked, setChecked] = useState(false);
  const [authed, setAuthed] = useState(false);

  useEffect(() => {
    let aborted = false;
    (async () => {
// 在三种状态之间决策：
        //   - 用户表为空 → /onboard
        //   - 用户存在，调用者有会话 → 渲染子组件
        //   - 用户存在，调用者无会话 → 显示 LoginScreen
      let configured = false;
      try {
        const res = await fetch("/api/status", { credentials: "same-origin" });
        if (res.ok) {
          const status = await res.json();
          configured = !!status.configured;
        }
      } catch {
        // 服务器不可用 — 落入 LoginScreen
      }
      if (aborted) return;

      if (!configured) {
        const onOnboard = pathname === "/onboard" || pathname.startsWith("/onboard/");
        if (!onOnboard) {
          router.replace("/onboard/");
          return;
        }
        setAuthed(true);
        setChecked(true);
        return;
      }

      // /signup 是管理员开放注册时的公开路由。允许其未认证渲染 ——
      // 页面本身会重新检查开关，如果管理员在页面加载和提交之间
      // 关闭了注册，会显示"注册已关闭"。
      if (pathname === "/signup" || pathname.startsWith("/signup/")) {
        setAuthed(true);
        setChecked(true);
        return;
      }

      try {
        const me = await getMe();
        if (me.ok && me.user) {
          if (isAdminPath(pathname) && me.user.role !== "super_admin") {
            router.replace("/overview/");
            return;
          }
          setAuthed(true);
        }
      } catch {
        // 网络错误 — 落入 LoginScreen
      }
      if (!aborted) setChecked(true);
    })();
    return () => { aborted = true; };
  }, [router, pathname]);

  if (!checked) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-zinc-950">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-zinc-700 border-t-violet-500" />
      </div>
    );
  }
  if (!authed) {
    return <LoginScreen onSuccess={() => setAuthed(true)} />;
  }
  return <>{children}</>;
}
