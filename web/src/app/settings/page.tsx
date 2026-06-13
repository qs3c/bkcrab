"use client";

import { redirect } from "next/navigation";

// /settings 是布局外壳；实际页面位于
// /settings/general、/settings/account、/settings/runtime。将
// 裸路径重定向到常规页 — 所有访问者均可访问（无需管理员权限）。
export default function SettingsIndex() {
  redirect("/settings/general");
}
