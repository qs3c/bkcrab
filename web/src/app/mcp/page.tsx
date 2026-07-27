"use client";

import * as React from "react";
import {
  AlertCircle,
  Cable,
  CheckCircle2,
  Loader2,
  Pencil,
  Plus,
  ServerCog,
  Trash2,
  Wrench,
} from "lucide-react";

import {
  createMCPResource,
  deleteMCPResource,
  listMCPResources,
  testMCPResource,
  updateMCPResource,
  type MCPGatewayStatus,
  type MCPResource,
  type MCPResourceInput,
  type MCPToolPreview,
} from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";

const SECRET_MASK = "********";

type MCPForm = {
  name: string;
  description: string;
  enabled: boolean;
  type: "stdio" | "http";
  transport: "sse" | "streamable-http";
  command: string;
  args: string;
  env: string;
  url: string;
  bearerToken: string;
};

const EMPTY_FORM: MCPForm = {
  name: "",
  description: "",
  enabled: true,
  type: "stdio",
  transport: "streamable-http",
  command: "",
  args: "",
  env: "",
  url: "",
  bearerToken: "",
};

function mapToText(values?: Record<string, string>): string {
  return Object.entries(values || {}).map(([key, value]) => `${key}=${value}`).join("\n");
}

function textToMap(value: string): Record<string, string> {
  const result: Record<string, string> = {};
  for (const rawLine of value.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line) continue;
    const separator = line.indexOf("=");
    if (separator <= 0) continue;
    result[line.slice(0, separator).trim()] = line.slice(separator + 1);
  }
  return result;
}

function formFromResource(resource: MCPResource): MCPForm {
  const authorization = resource.config.headers?.Authorization || "";
  return {
    name: resource.name,
    description: resource.description || "",
    enabled: resource.enabled,
    type: resource.config.type,
    transport: resource.config.transport || "streamable-http",
    command: resource.config.command || "",
    args: (resource.config.args || []).join("\n"),
    env: mapToText(resource.config.env),
    url: resource.config.url || "",
    bearerToken: authorization === SECRET_MASK
      ? SECRET_MASK
      : authorization.replace(/^Bearer\s+/i, ""),
  };
}

function payloadFromForm(form: MCPForm): MCPResourceInput {
  const args = form.args.split(/\r?\n/).map((item) => item.trim()).filter(Boolean);
  const headers = form.bearerToken.trim()
    ? {
        Authorization: form.bearerToken.trim() === SECRET_MASK
          ? SECRET_MASK
          : `Bearer ${form.bearerToken.trim()}`,
      }
    : undefined;
  return {
    name: form.name.trim(),
    description: form.description.trim(),
    enabled: form.enabled,
    config: {
      type: form.type,
      transport: form.transport,
      command: form.type === "stdio" ? form.command.trim() : undefined,
      args: form.type === "stdio" ? args : undefined,
      env: textToMap(form.env),
      url: form.type === "http" ? form.url.trim() : undefined,
      headers: form.type === "http" ? headers : undefined,
    },
  };
}

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

function gatewayLabel(gateway?: MCPGatewayStatus): string {
  switch (gateway?.status) {
    case "running":
      return "运行中";
    case "error":
      return "异常";
    case "stopped":
      return "未运行";
    default:
      return "待启动";
  }
}

export default function MCPPage() {
  const [resources, setResources] = React.useState<MCPResource[]>([]);
  const [gateway, setGateway] = React.useState<MCPGatewayStatus>();
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState("");
  const [notice, setNotice] = React.useState("");
  const [dialogOpen, setDialogOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<MCPResource | null>(null);
  const [form, setForm] = React.useState<MCPForm>(EMPTY_FORM);
  const [saving, setSaving] = React.useState(false);
  const [testingID, setTestingID] = React.useState("");
  const [testTools, setTestTools] = React.useState<Record<string, MCPToolPreview[]>>({});
  const [deleteTarget, setDeleteTarget] = React.useState<MCPResource | null>(null);
  const [deleting, setDeleting] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const response = await listMCPResources();
      setResources(response.servers || []);
      setGateway(response.gateway);
    } catch (err) {
      setError(errorMessage(err, "读取 MCP 服务失败"));
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  const openCreate = () => {
    setEditing(null);
    setForm(EMPTY_FORM);
    setDialogOpen(true);
  };

  const openEdit = (resource: MCPResource) => {
    setEditing(resource);
    setForm(formFromResource(resource));
    setDialogOpen(true);
  };

  const save = async () => {
    setSaving(true);
    setError("");
    try {
      const payload = payloadFromForm(form);
      if (editing) {
        await updateMCPResource(editing.id, payload);
        setNotice(`已更新 ${editing.name}`);
      } else {
        await createMCPResource(payload);
        setNotice(`已创建 ${payload.name}`);
      }
      setDialogOpen(false);
      await load();
    } catch (err) {
      setError(errorMessage(err, "保存 MCP 服务失败"));
    } finally {
      setSaving(false);
    }
  };

  const test = async (resource: MCPResource) => {
    setTestingID(resource.id);
    setError("");
    try {
      const response = await testMCPResource(resource.id);
      setTestTools((current) => ({ ...current, [resource.id]: response.tools || [] }));
      setNotice(`${resource.name} 连接成功，发现 ${response.tools?.length || 0} 个工具`);
      await load();
    } catch (err) {
      setError(errorMessage(err, `测试 ${resource.name} 失败`));
    } finally {
      setTestingID("");
    }
  };

  const remove = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    setError("");
    try {
      await deleteMCPResource(deleteTarget.id);
      setNotice(`已删除 ${deleteTarget.name}，相关 Agent 授权也已撤销`);
      setDeleteTarget(null);
      await load();
    } catch (err) {
      setError(errorMessage(err, "删除 MCP 服务失败"));
    } finally {
      setDeleting(false);
    }
  };

  return (
    <div className="mx-auto max-w-5xl space-y-6 p-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">MCP</h1>
          <p className="mt-1 max-w-2xl text-sm text-muted-foreground">
            在这里管理你的 MCP 服务，再到 Agent 设置中按需授权。连接凭据属于用户，不会写入 Agent 配置。
          </p>
        </div>
        <Button onClick={openCreate}>
          <Plus className="size-4" />
          添加 MCP 服务
        </Button>
      </div>

      {(error || notice) && (
        <div className={`flex items-start gap-2 rounded-lg border px-3 py-2.5 text-sm ${
          error
            ? "border-destructive/25 bg-destructive/10 text-destructive"
            : "border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400"
        }`}>
          {error ? <AlertCircle className="mt-0.5 size-4 shrink-0" /> : <CheckCircle2 className="mt-0.5 size-4 shrink-0" />}
          <span>{error || notice}</span>
        </div>
      )}

      <Card>
        <CardContent className="flex flex-wrap items-center gap-3 py-4">
          <span className="flex size-9 items-center justify-center rounded-lg bg-primary/10">
            <ServerCog className="size-4 text-primary" />
          </span>
          <div className="min-w-0 flex-1">
            <p className="text-sm font-medium">用户聚合网关</p>
            <p className="truncate text-xs text-muted-foreground">
              {gateway?.errorMessage || gateway?.baseUrl || "首次测试或使用 MCP 工具时自动启动"}
            </p>
          </div>
          <Badge variant={gateway?.status === "error" ? "destructive" : gateway?.status === "running" ? "default" : "outline"}>
            {gatewayLabel(gateway)}
          </Badge>
        </CardContent>
      </Card>

      {loading ? (
        <div className="grid gap-4 md:grid-cols-2">
          <Skeleton className="h-44 rounded-xl" />
          <Skeleton className="h-44 rounded-xl" />
        </div>
      ) : resources.length === 0 ? (
        <Card>
          <CardContent className="flex flex-col items-center gap-4 py-12 text-center">
            <span className="flex size-12 items-center justify-center rounded-xl bg-primary/10">
              <Cable className="size-6 text-primary" />
            </span>
            <div>
              <p className="font-medium">还没有 MCP 服务</p>
              <p className="mt-1 text-sm text-muted-foreground">添加后，可分别授权给一个或多个 Agent。</p>
            </div>
            <Button variant="outline" onClick={openCreate}>
              <Plus className="size-4" />
              添加第一个服务
            </Button>
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {resources.map((resource) => (
            <Card key={resource.id}>
              <CardHeader>
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <CardTitle className="flex flex-wrap items-center gap-2">
                      <span className="truncate">{resource.name}</span>
                      <Badge variant="outline">{resource.config.type}</Badge>
                      {!resource.enabled && <Badge variant="secondary">已停用</Badge>}
                    </CardTitle>
                    <CardDescription className="mt-1 line-clamp-2">
                      {resource.description || "未填写说明"}
                    </CardDescription>
                  </div>
                  <div className="flex shrink-0 items-center gap-1">
                    <Button variant="ghost" size="icon" onClick={() => openEdit(resource)} aria-label={`编辑 ${resource.name}`}>
                      <Pencil className="size-4" />
                    </Button>
                    <Button variant="ghost" size="icon" onClick={() => setDeleteTarget(resource)} aria-label={`删除 ${resource.name}`}>
                      <Trash2 className="size-4" />
                    </Button>
                  </div>
                </div>
              </CardHeader>
              <CardContent className="space-y-3">
                <p className="truncate rounded-md bg-muted px-2.5 py-2 font-mono text-xs text-muted-foreground">
                  {resource.config.type === "http"
                    ? resource.config.url
                    : [resource.config.command, ...(resource.config.args || [])].filter(Boolean).join(" ")}
                </p>
                {(testTools[resource.id]?.length || 0) > 0 && (
                  <div className="flex flex-wrap gap-1.5">
                    {testTools[resource.id].map((tool) => (
                      <Badge key={tool.name} variant="secondary" className="font-mono text-[10px]">
                        {tool.name}
                      </Badge>
                    ))}
                  </div>
                )}
                <Button
                  variant="outline"
                  className="w-full"
                  disabled={!resource.enabled || testingID === resource.id}
                  onClick={() => void test(resource)}
                >
                  {testingID === resource.id ? <Loader2 className="size-4 animate-spin" /> : <Wrench className="size-4" />}
                  {testingID === resource.id ? "正在连接" : "测试连接与工具"}
                </Button>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>{editing ? `编辑 ${editing.name}` : "添加 MCP 服务"}</DialogTitle>
            <DialogDescription>
              密钥会保存在用户级资源中；Agent 只保存这条资源的 ID。
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="grid gap-2">
              <Label htmlFor="mcp-name">名称</Label>
              <Input
                id="mcp-name"
                value={form.name}
                disabled={!!editing}
                placeholder="github"
                onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))}
              />
              <p className="text-xs text-muted-foreground">仅支持字母、数字、下划线和连字符，创建后不可修改。</p>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="mcp-description">说明</Label>
              <Input
                id="mcp-description"
                value={form.description}
                placeholder="例如：读取公司 GitHub 仓库"
                onChange={(event) => setForm((current) => ({ ...current, description: event.target.value }))}
              />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="grid gap-2">
                <Label>连接类型</Label>
                <Select value={form.type} onValueChange={(value) => value && setForm((current) => ({ ...current, type: value }))}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="stdio">本地命令（stdio）</SelectItem>
                    <SelectItem value="http">远程地址（HTTP）</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="grid gap-2">
                <Label>传输协议</Label>
                <Select value={form.transport} onValueChange={(value) => value && setForm((current) => ({ ...current, transport: value }))}>
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="streamable-http">Streamable HTTP</SelectItem>
                    <SelectItem value="sse">SSE</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
            {form.type === "stdio" ? (
              <>
                <div className="grid gap-2">
                  <Label htmlFor="mcp-command">命令</Label>
                  <Input id="mcp-command" value={form.command} placeholder="npx" onChange={(event) => setForm((current) => ({ ...current, command: event.target.value }))} />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="mcp-args">参数（每行一个）</Label>
                  <Textarea id="mcp-args" value={form.args} placeholder={"-y\n@modelcontextprotocol/server-github"} onChange={(event) => setForm((current) => ({ ...current, args: event.target.value }))} />
                </div>
              </>
            ) : (
              <>
                <div className="grid gap-2">
                  <Label htmlFor="mcp-url">服务地址</Label>
                  <Input id="mcp-url" value={form.url} placeholder="https://example.com/mcp" onChange={(event) => setForm((current) => ({ ...current, url: event.target.value }))} />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="mcp-token">Bearer Token（可选）</Label>
                  <Input id="mcp-token" type="password" value={form.bearerToken} placeholder="留空表示不认证" onChange={(event) => setForm((current) => ({ ...current, bearerToken: event.target.value }))} />
                </div>
              </>
            )}
            <div className="grid gap-2">
              <Label htmlFor="mcp-env">环境变量（每行 KEY=VALUE）</Label>
              <Textarea id="mcp-env" value={form.env} placeholder="GITHUB_TOKEN=..." onChange={(event) => setForm((current) => ({ ...current, env: event.target.value }))} />
            </div>
            <div className="flex items-center justify-between rounded-lg border px-3 py-2.5">
              <div>
                <Label htmlFor="mcp-enabled">启用服务</Label>
                <p className="text-xs text-muted-foreground">停用后，所有 Agent 都无法调用它。</p>
              </div>
              <Switch id="mcp-enabled" checked={form.enabled} onCheckedChange={(enabled) => setForm((current) => ({ ...current, enabled }))} />
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>取消</Button>
            <Button disabled={saving || !form.name.trim()} onClick={() => void save()}>
              {saving && <Loader2 className="size-4 animate-spin" />}
              {saving ? "保存中" : "保存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={!!deleteTarget} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除 {deleteTarget?.name}？</AlertDialogTitle>
            <AlertDialogDescription>
              这会删除连接配置和密钥，并自动从所有 Agent 的授权列表中移除它。此操作无法撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>取消</AlertDialogCancel>
            <AlertDialogAction disabled={deleting} onClick={(event) => { event.preventDefault(); void remove(); }}>
              {deleting && <Loader2 className="size-4 animate-spin" />}
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
