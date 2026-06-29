"use client";

import { createContext, useCallback, useContext, useEffect, useState } from "react";

export type Theme = "dark" | "light" | "system";

const STORAGE_KEY = "bkcrab-theme";

const ThemeContext = createContext<{
  theme: Theme;
  setTheme: (t: Theme) => void;
  toggleTheme: () => void;
  resolvedTheme: "dark" | "light";
}>({
  theme: "dark",
  setTheme: () => {},
  toggleTheme: () => {},
  resolvedTheme: "dark",
});

export function useTheme() {
  return useContext(ThemeContext);
}

function readSystem(): "dark" | "light" {
  if (typeof window === "undefined") return "dark";
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function apply(resolved: "dark" | "light") {
  document.documentElement.classList.toggle("dark", resolved === "dark");
}

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [theme, setThemeState] = useState<Theme>("dark");
  const [resolvedTheme, setResolvedTheme] = useState<"dark" | "light">("dark");

  useEffect(() => {
    // 挂载时从 localStorage 恢复。此处 setState 是合适的 ——
    // localStorage 在服务端不可访问（因此 useState 惰性初始化器
    // 会使 SSR 崩溃），我们希望首次绘制仅做一次到持久化主题的切换。
    const stored = localStorage.getItem(STORAGE_KEY) as Theme | null;
    const initial: Theme = stored === "light" || stored === "dark" || stored === "system" ? stored : "dark";
    setThemeState(initial);
    const resolved = initial === "system" ? readSystem() : initial;
    setResolvedTheme(resolved);
    apply(resolved);
  }, []);

  // 当 theme=system 时，实时跟随操作系统变化，使用户无需重新加载
  // 即可适应 macOS 自动主题的日落/日出。
  useEffect(() => {
    if (theme !== "system" || typeof window === "undefined") return;
    const mql = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => {
      const next = mql.matches ? "dark" : "light";
      setResolvedTheme(next);
      apply(next);
    };
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, [theme]);

  const setTheme = useCallback((next: Theme) => {
    setThemeState(next);
    localStorage.setItem(STORAGE_KEY, next);
    const resolved = next === "system" ? readSystem() : next;
    setResolvedTheme(resolved);
    apply(resolved);
  }, []);

  // toggleTheme 为现有导航用户下拉菜单保留 —— 在 dark → light → dark
  // 间循环；"system" 只能从 /settings 选择。
  const toggleTheme = useCallback(() => {
    setTheme(resolvedTheme === "dark" ? "light" : "dark");
  }, [resolvedTheme, setTheme]);

  return (
    <ThemeContext.Provider value={{ theme, setTheme, toggleTheme, resolvedTheme }}>
      {children}
    </ThemeContext.Provider>
  );
}
