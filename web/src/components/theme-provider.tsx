"use client";

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useSyncExternalStore,
} from "react";

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

const themeListeners = new Set<() => void>();

function readStoredTheme(): Theme {
  const stored = localStorage.getItem(STORAGE_KEY);
  return stored === "light" || stored === "dark" || stored === "system"
    ? stored
    : "dark";
}

function subscribeTheme(onStoreChange: () => void) {
  const onStorage = (event: StorageEvent) => {
    if (event.key === STORAGE_KEY) onStoreChange();
  };
  themeListeners.add(onStoreChange);
  window.addEventListener("storage", onStorage);
  return () => {
    themeListeners.delete(onStoreChange);
    window.removeEventListener("storage", onStorage);
  };
}

function subscribeSystem(onStoreChange: () => void) {
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  media.addEventListener("change", onStoreChange);
  return () => media.removeEventListener("change", onStoreChange);
}

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const theme = useSyncExternalStore<Theme>(
    subscribeTheme,
    readStoredTheme,
    () => "dark",
  );
  const systemTheme = useSyncExternalStore<"dark" | "light">(
    subscribeSystem,
    readSystem,
    () => "dark",
  );
  const resolvedTheme = theme === "system" ? systemTheme : theme;

  useEffect(() => {
    apply(resolvedTheme);
  }, [resolvedTheme]);

  const setTheme = useCallback((next: Theme) => {
    localStorage.setItem(STORAGE_KEY, next);
    const resolved = next === "system" ? readSystem() : next;
    apply(resolved);
    themeListeners.forEach((notify) => notify());
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
