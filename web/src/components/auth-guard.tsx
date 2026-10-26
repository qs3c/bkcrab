"use client";

import { useState, useEffect } from "react";
import { useRouter, usePathname } from "next/navigation";
import { getMe, getStatus, type MeResponse } from "@/lib/api";
import { Button } from "./ui/button";
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
  const [identity, setIdentity] = useState<MeResponse["user"] | null>(null);
  const [configured, setConfigured] = useState<boolean | null>(null);
  const [checked, setChecked] = useState(false);
  const [unavailable, setUnavailable] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const onSignup = /^\/signup\/?$/.test(pathname);
  const onOnboard = /^\/onboard(?:\/|$)/.test(pathname);

  useEffect(() => {
    const refresh = () => { setChecked(false); setAttempt((n) => n + 1); };
    window.addEventListener("bkcrab:authenticated", refresh);
    return () => window.removeEventListener("bkcrab:authenticated", refresh);
  }, []);

  useEffect(() => {
    let aborted = false;
    (async () => {
// 在三种状态之间决策：
        //   - 用户表为空 → /onboard
        //   - 用户存在，调用者有会话 → 渲染子组件
        //   - 用户存在，调用者无会话 → 显示 LoginScreen
      setUnavailable(false);
      let configured: boolean;
      try {
        const status = await getStatus();
        configured = status.configured;
      } catch {
        if (!aborted) setUnavailable(true);
        return;
      }
      if (aborted) return;
      setConfigured(configured);

      if (!configured) {
        const onOnboard = pathname === "/onboard" || pathname.startsWith("/onboard/");
        if (!onOnboard) {
          router.replace("/onboard/");
          return;
        }
        setChecked(true);
        return;
      }

      // /signup 是管理员开放注册时的公开路由。允许其未认证渲染 ——
      // 页面本身会重新检查开关，如果管理员在页面加载和提交之间
      // 关闭了注册，会显示"注册已关闭"。
      try {
        const me = await getMe();
        if (aborted) return;
        setIdentity(me.ok && me.user ? me.user : null);
        if (me.ok && me.user) {
          if (onOnboard || onSignup || (isAdminPath(pathname) && me.user.role !== "super_admin")) {
            router.replace("/overview/");
          }
        }
      } catch {
        if (!aborted) setUnavailable(true);
        return;
      }
      if (!aborted) setChecked(true);
    })();
    return () => { aborted = true; };
  }, [router, pathname, attempt, onOnboard, onSignup]);

  if (unavailable) {
    return (
      <div className="flex min-h-svh items-center justify-center bg-background p-6">
        <div className="max-w-sm space-y-4 text-center" role="alert">
          <h1 className="text-xl font-semibold">暂时无法连接服务器</h1>
          <p className="text-sm text-muted-foreground">请检查网络后重试，你的账户和已有内容不会被重置。</p>
          <Button onClick={() => setAttempt((n) => n + 1)}>重新连接</Button>
        </div>
      </div>
    );
  }

  if (!checked) {
    return (
      <div className="flex min-h-svh items-center justify-center bg-background" role="status" aria-label="正在加载">
        <div className="h-8 w-8 animate-spin rounded-full border-2 border-zinc-700 border-t-violet-500" />
      </div>
    );
  }
  if (configured === false) return onOnboard ? <>{children}</> : null;
  if (!identity) {
    if (onSignup) return <>{children}</>;
    return <LoginScreen onSuccess={() => { setChecked(false); setAttempt((n) => n + 1); }} />;
  }
  if (isAdminPath(pathname) && identity.role !== "super_admin") return null;
  return <>{children}</>;
}
