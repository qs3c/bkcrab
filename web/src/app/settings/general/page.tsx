"use client";

import { Sun, Moon, Monitor } from "lucide-react";
import { useTheme, type Theme } from "@/components/theme-provider";

const choices: Array<{ value: Theme; label: string; icon: React.ComponentType<{ className?: string }> }> = [
  { value: "light", label: "Light", icon: Sun },
  { value: "dark", label: "Dark", icon: Moon },
  { value: "system", label: "系统", icon: Monitor },
];

export default function GeneralSettingsPage() {
  const { theme, setTheme } = useTheme();

  return (
    <div className="space-y-6">
      <div>
        <h3 className="text-xl font-semibold tracking-tight">常规</h3>
        <p className="text-sm text-muted-foreground mt-1">
          外观和当前设备偏好设置。
        </p>
      </div>

      <div className="rounded-lg border border-border bg-card p-5">
        <h4 className="font-medium mb-1">主题</h4>
        <p className="text-sm text-muted-foreground mb-4">
          选择仪表盘配色方案。“系统”将跟随操作系统设置。
        </p>
        <div className="grid grid-cols-3 gap-3 max-w-md">
          {choices.map((c) => {
            const active = theme === c.value;
            const Icon = c.icon;
            return (
              <button
                key={c.value}
                type="button"
                onClick={() => setTheme(c.value)}
                className={
                  "flex flex-col items-center gap-2 rounded-md border px-3 py-4 text-sm transition " +
                  (active
                    ? "border-primary bg-primary/10 text-primary"
                    : "border-border hover:bg-muted")
                }
              >
                <Icon className="size-5" />
                {c.label}
              </button>
            );
          })}
        </div>
      </div>
    </div>
  );
}
