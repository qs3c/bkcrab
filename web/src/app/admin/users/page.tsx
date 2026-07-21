"use client";

import { useEffect, useState } from "react";
import {
  adminListUsers,
  adminCreateUser,
  adminUpdateUser,
  adminDeleteUser,
  adminResetPassword,
  getRegistration,
  setRegistration,
} from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Card, CardContent } from "@/components/ui/card";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Users, KeyRound, Trash2, Plus } from "lucide-react";

interface UserRow {
  id: string;
  username: string;
  email: string;
  displayName?: string;
  role: string;
  status: string;
}

export default function AdminUsersPage() {
  const [users, setUsers] = useState<UserRow[]>([]);
  const [error, setError] = useState("");
  const [createOpen, setCreateOpen] = useState(false);
  const [form, setForm] = useState({
    username: "",
    email: "",
    password: "",
    displayName: "",
    role: "user",
  });

  const [deleteTarget, setDeleteTarget] = useState<UserRow | null>(null);
  const [resetTarget, setResetTarget] = useState<UserRow | null>(null);
  const [resetPwd, setResetPwd] = useState("");
  const [regOpen, setRegOpen] = useState<boolean | null>(null);

  async function refresh() {
    const res = await adminListUsers();
    setError("");
    if (res.users) setUsers(res.users);
    if (res.error) setError(res.error);
  }
  useEffect(() => {
    let cancelled = false;
    adminListUsers().then((res) => {
      if (cancelled) return;
      setError("");
      if (res.users) setUsers(res.users);
      if (res.error) setError(res.error);
    });
    getRegistration()
      .then((r) => {
        if (!cancelled) setRegOpen(!!r.open);
      })
      .catch(() => {
        if (!cancelled) setRegOpen(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  async function toggleRegistration(next: boolean) {
    // 乐观翻转；失败时回滚，确保 UI 永不谎报后端状态。
    setRegOpen(next);
    try {
      const r = await setRegistration(next);
      setRegOpen(!!r.open);
    } catch {
      setRegOpen(!next);
      setError("更新注册设置失败");
    }
  }

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    const res = await adminCreateUser(form);
    if (res.error) {
      setError(res.error);
      return;
    }
    setCreateOpen(false);
    setForm({ username: "", email: "", password: "", displayName: "", role: "user" });
    refresh();
  }

  async function setRole(u: UserRow, role: string) {
    setError("");
    const res = await adminUpdateUser(u.id, { role });
    if (res.error) setError(res.error);
    refresh();
  }

  async function setStatus(u: UserRow, status: string) {
    setError("");
    const res = await adminUpdateUser(u.id, { status });
    if (res.error) setError(res.error);
    refresh();
  }

  async function handleResetPassword() {
    if (!resetTarget || !resetPwd.trim()) return;
    const res = await adminResetPassword(resetTarget.id, resetPwd);
    if (res.error) {
      setError(res.error);
      return;
    }
    setResetTarget(null);
    setResetPwd("");
  }

  async function handleDelete(u: UserRow) {
    const res = await adminDeleteUser(u.id);
    if (res.error) setError(res.error);
    setDeleteTarget(null);
    refresh();
  }

  function openCreateDialog() {
    setForm({ username: "", email: "", password: "", displayName: "", role: "user" });
    setError("");
    setCreateOpen(true);
  }

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-2xl font-semibold tracking-tight">用户</h2>
          <p className="text-sm text-muted-foreground mt-1">
            管理平台成员。每位用户都拥有相互隔离的智能体、会话和密钥。
          </p>
        </div>
        <Button onClick={openCreateDialog}>
          <Plus className="h-4 w-4 mr-2" />
          添加用户
        </Button>
      </div>

      <Card>
        <CardContent>
          <div className="flex items-center justify-between gap-4">
            <div className="space-y-1">
              <p className="text-sm font-medium">开放注册</p>
              <p className="text-xs text-muted-foreground">
                开启后，任何获得 URL 的人都可通过 /signup 创建账户。
                关闭后，只有你能在此页面添加用户。
              </p>
            </div>
            <Switch
              checked={!!regOpen}
              onCheckedChange={toggleRegistration}
              disabled={regOpen === null}
              aria-label="切换公开注册"
            />
          </div>
        </CardContent>
      </Card>

      {error && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="pt-6">
            <p className="text-sm text-destructive">{error}</p>
          </CardContent>
        </Card>
      )}

      {users.length === 0 ? (
        <div className="rounded-lg border border-border bg-card">
          <div className="flex flex-col items-center justify-center py-16">
            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-primary/10 mb-4">
              <Users className="h-7 w-7 text-primary" />
            </div>
            <p className="text-sm text-muted-foreground mb-1">暂无用户</p>
            <p className="text-xs text-muted-foreground/60 mb-4">
              添加用户，为其创建独立范围的工作区
            </p>
            <Button variant="outline" size="sm" onClick={openCreateDialog}>
              <Plus className="h-4 w-4 mr-2" />
              添加用户
            </Button>
          </div>
        </div>
      ) : (
        <div className="rounded-lg border border-border bg-card overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>用户名</TableHead>
                <TableHead>邮箱</TableHead>
                <TableHead>角色</TableHead>
                <TableHead>状态</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.map((u) => (
                <TableRow key={u.id}>
                  <TableCell className="font-medium">
                    <div>{u.username}</div>
                    {u.displayName && (
                      <div className="text-xs text-muted-foreground">{u.displayName}</div>
                    )}
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{u.email}</TableCell>
                  <TableCell>
                    <Select value={u.role} onValueChange={(v) => v && setRole(u, v)}>
                      <SelectTrigger size="sm" className="w-36">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="user">用户</SelectItem>
                        <SelectItem value="super_admin">超级管理员</SelectItem>
                      </SelectContent>
                    </Select>
                  </TableCell>
                  <TableCell>
                    <Select value={u.status} onValueChange={(v) => v && setStatus(u, v)}>
                      <SelectTrigger size="sm" className="w-32">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="active">启用</SelectItem>
                        <SelectItem value="disabled">禁用</SelectItem>
                      </SelectContent>
                    </Select>
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="icon"
                        variant="ghost"
                        onClick={() => {
                          setResetPwd("");
                          setResetTarget(u);
                        }}
                        title="重置密码"
                      >
                        <KeyRound className="size-4" />
                      </Button>
                      <Button
                        size="icon"
                        variant="ghost"
                        className="text-destructive hover:text-destructive"
                        onClick={() => setDeleteTarget(u)}
                        title="删除"
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>添加用户</DialogTitle>
            <DialogDescription>
              创建新的平台成员。他们将拥有独立范围的智能体、会话和密钥。
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleCreate} className="space-y-4 py-2">
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="user-username">用户名</Label>
                <Input
                  id="user-username"
                  required
                  value={form.username}
                  onChange={(e) => setForm({ ...form, username: e.target.value })}
                  placeholder="例如 alice"
                  autoFocus
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="user-email">邮箱</Label>
                <Input
                  id="user-email"
                  required
                  type="email"
                  value={form.email}
                  onChange={(e) => setForm({ ...form, email: e.target.value })}
                  placeholder="alice@example.com"
                />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="user-password">密码</Label>
              <Input
                id="user-password"
                required
                type="password"
                value={form.password}
                onChange={(e) => setForm({ ...form, password: e.target.value })}
                placeholder="初始密码"
              />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="user-display">显示名称</Label>
                <Input
                  id="user-display"
                  value={form.displayName}
                  onChange={(e) => setForm({ ...form, displayName: e.target.value })}
                  placeholder="可选"
                />
              </div>
              <div className="space-y-1.5">
                <Label>角色</Label>
                <Select
                  value={form.role}
                  onValueChange={(v) => v && setForm({ ...form, role: v })}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="user">用户</SelectItem>
                    <SelectItem value="super_admin">超级管理员</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>
                取消
              </Button>
              <Button
                type="submit"
                disabled={!form.username.trim() || !form.email.trim() || !form.password.trim()}
              >
                创建用户
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <Dialog
        open={resetTarget !== null}
        onOpenChange={(o) => {
          if (!o) {
            setResetTarget(null);
            setResetPwd("");
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>重置密码</DialogTitle>
            <DialogDescription>
              为以下用户设置新密码：{" "}
              <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                {resetTarget?.username}
              </code>
              。他们下次登录时需要使用此密码。
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-1.5 py-2">
            <Label htmlFor="reset-pwd">新密码</Label>
            <Input
              id="reset-pwd"
              type="password"
              value={resetPwd}
              onChange={(e) => setResetPwd(e.target.value)}
              autoFocus
            />
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => {
                setResetTarget(null);
                setResetPwd("");
              }}
            >
              取消
            </Button>
            <Button onClick={handleResetPassword} disabled={!resetPwd.trim()}>
              重置密码
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(o) => !o && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除用户？</AlertDialogTitle>
            <AlertDialogDescription>
              <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                {deleteTarget?.username}
              </code>{" "}
              及其全部智能体、会话和 API 密钥都将被删除。此操作无法撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => deleteTarget && handleDelete(deleteTarget)}>
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
