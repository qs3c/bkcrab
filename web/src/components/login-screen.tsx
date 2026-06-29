"use client";

import { useEffect, useState } from "react";
import { login as apiLogin, register, getStatus } from "@/lib/api";

interface LoginScreenProps {
  onSuccess: () => void;
}

// LoginScreen 在登录和注册之间内联切换，而非导航到 /signup。
// 两个原因：(a) 用户着陆的智能体分享 URL 在整个流程中保持不变，
// 因此注册成功后直接到达他们想要的页面；(b) /signup 作为独立路由
// 会在 AppShell 的 SidebarLayout 中渲染，向尚未注册的访客泄漏了
// 已认证的应用框架。服务端注册会设置会话 cookie，因此注册成功
// 在功能上等同于登录成功 —— 两者都通过 `onSuccess` 路由，由
// AuthGuard 渲染最初请求的页面。
export function LoginScreen({ onSuccess }: LoginScreenProps) {
  const [mode, setMode] = useState<"signin" | "signup">("signin");
  const [loginField, setLoginField] = useState("");
  const [password, setPassword] = useState("");
  const [signupUsername, setSignupUsername] = useState("");
  const [signupEmail, setSignupEmail] = useState("");
  const [signupConfirm, setSignupConfirm] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [registrationOpen, setRegistrationOpen] = useState(false);

  useEffect(() => {
    let aborted = false;
    getStatus()
      .then((s) => { if (!aborted) setRegistrationOpen(!!s.registrationOpen); })
      .catch(() => { /* 保持默认值 false — 注册链接保持隐藏 */ });
    return () => { aborted = true; };
  }, []);

  function switchMode(next: "signin" | "signup") {
    setError("");
    setMode(next);
  }

  async function handleSignIn(e: React.FormEvent) {
    e.preventDefault();
    if (!loginField.trim() || !password) return;
    setLoading(true);
    setError("");
    try {
      const res = await apiLogin(loginField.trim(), password);
      if (!res.ok) {
        setError(res.error || "用户名或密码错误");
        setLoading(false);
        return;
      }
      onSuccess();
    } catch {
      setError("无法连接服务器");
      setLoading(false);
    }
  }

  async function handleSignUp(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    if (!signupUsername.trim() || !signupEmail.trim() || !password) {
      setError("所有字段均为必填项");
      return;
    }
    if (password.length < 8) {
      setError("密码至少需要 8 个字符");
      return;
    }
    if (password !== signupConfirm) {
      setError("两次输入的密码不一致");
      return;
    }
    setLoading(true);
    try {
      const res = await register({
        username: signupUsername.trim(),
        email: signupEmail.trim(),
        password,
      });
      if (!res.ok) {
        setError(res.error || "无法创建账户");
        setLoading(false);
        return;
      }
      // 注册处理器已在响应上设置了会话 cookie，因此应用实际上
      // 已经登录。复用与登录相同的回调，AuthGuard 将渲染最初
      // 请求的路由，无需任何重定向。
      onSuccess();
    } catch {
      setError("无法连接服务器");
      setLoading(false);
    }
  }

  if (mode === "signup") {
    return (
      <div className="flex min-h-screen items-center justify-center bg-zinc-950 p-4">
        <div className="w-full max-w-sm space-y-6">
          <div className="text-center space-y-2">
            <h1 className="text-2xl font-bold text-zinc-100">创建你的账户</h1>
            <p className="text-sm text-zinc-500">注册后即可开始使用 BkCrab</p>
          </div>
          <form onSubmit={handleSignUp} className="space-y-4">
            <input
              type="text"
              value={signupUsername}
              onChange={(e) => setSignupUsername(e.target.value)}
              placeholder="用户名"
              autoFocus
              autoComplete="username"
              className="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-4 py-3 text-sm text-zinc-100 placeholder-zinc-600 outline-none focus:border-violet-500 focus:ring-1 focus:ring-violet-500"
            />
            <input
              type="email"
              value={signupEmail}
              onChange={(e) => setSignupEmail(e.target.value)}
              placeholder="邮箱"
              autoComplete="email"
              className="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-4 py-3 text-sm text-zinc-100 placeholder-zinc-600 outline-none focus:border-violet-500 focus:ring-1 focus:ring-violet-500"
            />
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="密码（至少 8 个字符）"
              autoComplete="new-password"
              className="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-4 py-3 text-sm text-zinc-100 placeholder-zinc-600 outline-none focus:border-violet-500 focus:ring-1 focus:ring-violet-500"
            />
            <input
              type="password"
              value={signupConfirm}
              onChange={(e) => setSignupConfirm(e.target.value)}
              placeholder="确认密码"
              autoComplete="new-password"
              className="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-4 py-3 text-sm text-zinc-100 placeholder-zinc-600 outline-none focus:border-violet-500 focus:ring-1 focus:ring-violet-500"
            />
            {error && <p className="text-sm text-red-400">{error}</p>}
            <button
              type="submit"
              disabled={loading || !signupUsername.trim() || !signupEmail.trim() || !password || !signupConfirm}
              className="w-full rounded-lg bg-violet-600 px-4 py-3 text-sm font-medium text-white transition hover:bg-violet-500 disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {loading ? "正在创建账户..." : "创建账户"}
            </button>
          </form>
          <p className="text-center text-sm text-zinc-500">
            已有账户？{" "}
            <button
              type="button"
              onClick={() => switchMode("signin")}
              className="text-violet-400 hover:text-violet-300"
            >
              登录
            </button>
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-zinc-950 p-4">
      <div className="w-full max-w-sm space-y-6">
        <div className="text-center space-y-2">
          <h1 className="text-2xl font-bold text-zinc-100">BkCrab</h1>
          <p className="text-sm text-zinc-500">使用用户名或邮箱登录</p>
        </div>
        <form onSubmit={handleSignIn} className="space-y-4">
          <input
            type="text"
            value={loginField}
            onChange={(e) => setLoginField(e.target.value)}
            placeholder="用户名或邮箱"
            autoFocus
            autoComplete="username"
            className="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-4 py-3 text-sm text-zinc-100 placeholder-zinc-600 outline-none focus:border-violet-500 focus:ring-1 focus:ring-violet-500"
          />
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="密码"
            autoComplete="current-password"
            className="w-full rounded-lg border border-zinc-800 bg-zinc-900 px-4 py-3 text-sm text-zinc-100 placeholder-zinc-600 outline-none focus:border-violet-500 focus:ring-1 focus:ring-violet-500"
          />
          {error && <p className="text-sm text-red-400">{error}</p>}
          <button
            type="submit"
            disabled={loading || !loginField.trim() || !password}
            className="w-full rounded-lg bg-violet-600 px-4 py-3 text-sm font-medium text-white transition hover:bg-violet-500 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {loading ? "正在登录..." : "登录"}
          </button>
        </form>
        {registrationOpen && (
          <p className="text-center text-sm text-zinc-500">
            还没有账户？{" "}
            <button
              type="button"
              onClick={() => switchMode("signup")}
              className="text-violet-400 hover:text-violet-300"
            >
              注册
            </button>
          </p>
        )}
      </div>
    </div>
  );
}
