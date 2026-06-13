// 基于 Cookie 的会话是权威的登录状态来源。这些辅助函数的存在
// 是为了让仍然引用 isLoggedIn / logout 的旧调用方继续工作，
// 同时 UI 的其余部分已迁移到 /api/me + /api/login + /api/logout。
import { setAuthToken, getAuthToken, logout as apiLogout } from "./api";

// isLoggedIn 在 localStorage 中存在 bearer token 时返回 true。
// UI 的权威登录状态是 cookie 会话，但一些旧组件仍基于此来控制 UI
// 渲染——即使 isLoggedIn 为 false 它们也能正确渲染，
// 因为 apiFetch 会无条件发送 cookie。
export function isLoggedIn(): boolean {
  return !!getAuthToken();
}

export function login(token: string) {
  // 编程式客户端仍可在此处设置 bearer apikey；当两者同时存在时，
  // 服务端以 cookie 会话为准。
  setAuthToken(token);
}

export function logout() {
  void apiLogout();
}