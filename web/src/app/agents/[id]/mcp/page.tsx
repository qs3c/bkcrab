"use client";

import * as React from "react";
import {
  Cable,
  CheckCircle2,
  Loader2,
  Plus,
  RefreshCw,
  ServerCog,
  Trash2,
  XCircle,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import {
  getAgentMCPConfig,
  saveAgentMCPConfig,
  testAgentMCP,
  type MCPServerConfig,
  type MCPToolPreview,
} from "@/lib/api";

type MCPServerType = MCPServerConfig["type"];
type MCPTransport = NonNullable<MCPServerConfig["transport"]>;
type DraftServer = Omit<
  MCPServerConfig,
  "type" | "transport" | "enabled" | "args" | "env" | "headers"
> & {
  name: string;
  type: MCPServerType;
  transport: MCPTransport;
  enabled: boolean;
  args: string[];
  env: Record<string, string>;
  headers: Record<string, string>;
};

const emptyServer = (): DraftServer => ({
  name: "",
  type: "stdio",
  command: "",
  args: [],
  env: {},
  headers: {},
  transport: "streamable-http",
  enabled: true,
});

function mapToText(map?: Record<string, string>) {
  return Object.entries(map || {})
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");
}

function textToMap(text: string) {
  const out: Record<string, string> = {};
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line) continue;
    const idx = line.indexOf("=");
    if (idx <= 0) continue;
    out[line.slice(0, idx).trim()] = line.slice(idx + 1);
  }
  return out;
}

function serversToDrafts(input: Record<string, MCPServerConfig>): DraftServer[] {
  return Object.entries(input || {}).map(([name, cfg]) => ({
    name,
    type: cfg.type,
    url: cfg.url,
    headers: cfg.headers || {},
    command: cfg.command,
    args: cfg.args || [],
    env: cfg.env || {},
    transport: cfg.transport || "streamable-http",
    enabled: cfg.enabled !== false,
  }));
}

function draftsToServers(drafts: DraftServer[]): Record<string, MCPServerConfig> {
  const out: Record<string, MCPServerConfig> = {};
  for (const draft of drafts) {
    const name = draft.name.trim();
    if (!name) continue;
    out[name] = {
      type: draft.type,
      enabled: draft.enabled !== false,
      transport: draft.transport || "streamable-http",
      url: draft.type === "http" ? draft.url?.trim() : undefined,
      headers: draft.type === "http" ? draft.headers || {} : undefined,
      command: draft.type === "stdio" ? draft.command?.trim() : undefined,
      args: draft.type === "stdio" ? draft.args || [] : undefined,
      env: draft.env || {},
    };
  }
  return out;
}

function errorMessage(err: unknown, fallback: string) {
  return err instanceof Error ? err.message : fallback;
}

function nextServerName(servers: DraftServer[]) {
  const names = new Set(servers.map((s) => s.name));
  for (let i = 1; ; i += 1) {
    const candidate = `server-${i}`;
    if (!names.has(candidate)) return candidate;
  }
}

function hasDuplicateNames(servers: DraftServer[]) {
  const seen = new Set<string>();
  for (const server of servers) {
    const name = server.name.trim();
    if (!name) continue;
    if (seen.has(name)) return true;
    seen.add(name);
  }
  return false;
}

function gatewayBadgeVariant(status: string): "default" | "secondary" | "destructive" | "outline" {
  if (status === "running") return "default";
  if (status === "error") return "destructive";
  if (status === "stopped" || status === "unknown") return "outline";
  return "secondary";
}

export default function AgentMCPPage() {
  const agentId = useAgentIdFromURL();
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [shareMcpConfig, setShareMcpConfig] = React.useState(false);
  const [servers, setServers] = React.useState<DraftServer[]>([]);
  const [selected, setSelected] = React.useState(0);
  const [gatewayStatus, setGatewayStatus] = React.useState<string>("stopped");
  const [tools, setTools] = React.useState<MCPToolPreview[]>([]);
  const [message, setMessage] = React.useState<string>("");

  const load = React.useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    setMessage("");
    try {
      const res = await getAgentMCPConfig(agentId);
      setServers(serversToDrafts(res.mcpServers || {}));
      setShareMcpConfig(!!res.shareMcpConfig);
      setGatewayStatus(res.gateway?.status || "stopped");
      setSelected(0);
      setTools([]);
      if (res.error || res.gateway?.errorMessage) {
        setMessage(res.error || res.gateway?.errorMessage || "");
      }
    } catch (err) {
      setMessage(errorMessage(err, "加载 MCP 配置失败"));
    } finally {
      setLoading(false);
    }
  }, [agentId]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const selectedServer = servers[selected] || null;

  const updateSelected = (patch: Partial<DraftServer>) => {
    setServers((prev) =>
      prev.map((server, index) =>
        index === selected ? { ...server, ...patch } : server,
      ),
    );
  };

  const saveCurrent = async (quiet = false) => {
    if (!agentId) return false;
    if (hasDuplicateNames(servers)) {
      setMessage("服务器名称不能重复");
      return false;
    }
    setSaving(true);
    if (!quiet) setMessage("");
    try {
      const res = await saveAgentMCPConfig(agentId, {
        mcpServers: draftsToServers(servers),
        shareMcpConfig,
      });
      if (res.error) {
        setMessage(res.error);
        return false;
      }
      setServers(serversToDrafts(res.mcpServers || {}));
      setShareMcpConfig(!!res.shareMcpConfig);
      setGatewayStatus(res.gateway?.status || "stopped");
      if (!quiet) setMessage("已保存");
      return true;
    } catch (err) {
      setMessage(errorMessage(err, "保存 MCP 配置失败"));
      return false;
    } finally {
      setSaving(false);
    }
  };

  const handleTest = async () => {
    if (!agentId) return;
    setTesting(true);
    setTools([]);
    setMessage("");
    try {
      const saved = await saveCurrent(true);
      if (!saved) return;
      const res = await testAgentMCP(agentId);
      setTools(res.tools || []);
      setMessage(res.ok ? `测试通过：${res.tools?.length || 0} 个工具` : res.error || "测试失败");
    } catch (err) {
      setMessage(errorMessage(err, "测试 MCP 连接失败"));
    } finally {
      setTesting(false);
    }
  };

  const addServer = () => {
    const next = { ...emptyServer(), name: nextServerName(servers) };
    setServers((prev) => [...prev, next]);
    setSelected(servers.length);
    setMessage("");
  };

  const removeServer = (index: number) => {
    const next = servers.filter((_, i) => i !== index);
    setServers(next);
    setSelected(Math.min(selected, Math.max(next.length - 1, 0)));
    setMessage("");
  };

  if (loading) {
    return (
      <div className="p-6 space-y-4 max-w-6xl mx-auto">
        <Skeleton className="h-9 w-48" />
        <Skeleton className="h-[420px] w-full" />
      </div>
    );
  }

  return (
    <div className="p-6 space-y-5 max-w-6xl mx-auto">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <Cable className="size-5 text-muted-foreground" />
            <h2 className="text-2xl font-semibold tracking-tight">MCP</h2>
            <Badge variant={gatewayBadgeVariant(gatewayStatus)}>
              {gatewayStatus === "running" ? (
                <CheckCircle2 className="size-3" />
              ) : (
                <XCircle className="size-3" />
              )}
              {gatewayStatus || "stopped"}
            </Badge>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          <label className="flex h-8 items-center gap-2 rounded-lg border bg-background px-3 text-sm">
            <Switch
              size="sm"
              checked={shareMcpConfig}
              onCheckedChange={setShareMcpConfig}
            />
            共享给访客
          </label>
          <Button variant="outline" size="sm" onClick={addServer}>
            <Plus className="size-4" />
            添加
          </Button>
          <Button variant="outline" size="sm" onClick={load} disabled={saving || testing}>
            <RefreshCw className="size-4" />
            刷新
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void saveCurrent()}
            disabled={saving || testing}
          >
            {saving ? <Loader2 className="size-4 animate-spin" /> : <CheckCircle2 className="size-4" />}
            保存
          </Button>
          <Button size="sm" onClick={handleTest} disabled={saving || testing}>
            {testing ? <Loader2 className="size-4 animate-spin" /> : <ServerCog className="size-4" />}
            测试
          </Button>
        </div>
      </div>

      {message && (
        <div className="rounded-lg border border-border bg-muted/30 px-3 py-2 text-sm text-muted-foreground">
          {message}
        </div>
      )}

      <div className="grid gap-4 lg:grid-cols-[320px_1fr]">
        <section className="rounded-lg border bg-card">
          <div className="flex h-11 items-center justify-between border-b px-3">
            <span className="text-sm font-medium">服务器</span>
            <Badge variant="outline">{servers.length}</Badge>
          </div>
          <div className="divide-y">
            {servers.length === 0 ? (
              <div className="flex min-h-[180px] flex-col items-center justify-center gap-3 p-6 text-center text-sm text-muted-foreground">
                <ServerCog className="size-7" />
                未配置 MCP 服务器
              </div>
            ) : (
              servers.map((server, index) => (
                <div
                  key={`${server.name}-${index}`}
                  role="button"
                  tabIndex={0}
                  onClick={() => setSelected(index)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      setSelected(index);
                    }
                  }}
                  className={`grid min-h-12 w-full grid-cols-[1fr_auto_auto] items-center gap-2 px-3 py-2 text-left text-sm transition-colors ${
                    selected === index ? "bg-muted" : "hover:bg-muted/50"
                  }`}
                >
                  <span className="min-w-0">
                    <span className="block truncate font-medium">
                      {server.name || "未命名"}
                    </span>
                    <span className="block truncate text-xs text-muted-foreground">
                      {server.type}
                      {server.type === "http" && server.url ? ` · ${server.url}` : ""}
                      {server.type === "stdio" && server.command ? ` · ${server.command}` : ""}
                    </span>
                  </span>
                  <Badge variant={server.enabled ? "secondary" : "outline"}>
                    {server.enabled ? "启用" : "关闭"}
                  </Badge>
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon-sm"
                    title="删除"
                    aria-label="删除 MCP 服务器"
                    onClick={(event) => {
                      event.stopPropagation();
                      removeServer(index);
                    }}
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </div>
              ))
            )}
          </div>
        </section>

        <section className="rounded-lg border bg-card">
          <div className="flex h-11 items-center justify-between border-b px-3">
            <span className="text-sm font-medium">编辑</span>
            {selectedServer && (
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <Switch
                  size="sm"
                  checked={selectedServer.enabled}
                  onCheckedChange={(value) => updateSelected({ enabled: value })}
                />
                启用
              </div>
            )}
          </div>

          {selectedServer ? (
            <div className="grid gap-4 p-4 md:grid-cols-2">
              <Field label="名称">
                <Input
                  value={selectedServer.name}
                  onChange={(event) => updateSelected({ name: event.target.value })}
                  placeholder="time"
                  className="font-mono"
                />
              </Field>

              <Field label="类型">
                <Select
                  value={selectedServer.type}
                  onValueChange={(value: string | null) =>
                    value && updateSelected({ type: value as MCPServerType })
                  }
                >
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="stdio">stdio</SelectItem>
                    <SelectItem value="http">http</SelectItem>
                  </SelectContent>
                </Select>
              </Field>

              <Field label="传输">
                <Select
                  value={selectedServer.transport}
                  onValueChange={(value: string | null) =>
                    value && updateSelected({ transport: value as MCPTransport })
                  }
                >
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="streamable-http">streamable-http</SelectItem>
                    <SelectItem value="sse">sse</SelectItem>
                  </SelectContent>
                </Select>
              </Field>

              {selectedServer.type === "http" ? (
                <>
                  <Field label="URL" className="md:col-span-2">
                    <Input
                      value={selectedServer.url || ""}
                      onChange={(event) => updateSelected({ url: event.target.value })}
                      placeholder="https://example.com/mcp"
                      className="font-mono"
                    />
                  </Field>
                  <Field label="Authorization" className="md:col-span-2">
                    <Input
                      type="password"
                      value={selectedServer.headers.Authorization || ""}
                      onChange={(event) =>
                        updateSelected({
                          headers: event.target.value
                            ? { Authorization: event.target.value }
                            : {},
                        })
                      }
                      placeholder="Bearer ..."
                      className="font-mono"
                    />
                  </Field>
                </>
              ) : (
                <>
                  <Field label="命令" className="md:col-span-2">
                    <Input
                      value={selectedServer.command || ""}
                      onChange={(event) => updateSelected({ command: event.target.value })}
                      placeholder="uvx"
                      className="font-mono"
                    />
                  </Field>
                  <Field label="参数" className="md:col-span-2">
                    <Textarea
                      value={selectedServer.args.join("\n")}
                      onChange={(event) =>
                        updateSelected({
                          args: event.target.value
                            .split(/\r?\n/)
                            .map((line) => line.trim())
                            .filter(Boolean),
                        })
                      }
                      placeholder="mcp-server-time"
                      className="min-h-24 font-mono"
                    />
                  </Field>
                </>
              )}

              <Field label="环境变量" className="md:col-span-2">
                <Textarea
                  value={mapToText(selectedServer.env)}
                  onChange={(event) => updateSelected({ env: textToMap(event.target.value) })}
                  placeholder="KEY=value"
                  className="min-h-28 font-mono"
                />
              </Field>
            </div>
          ) : (
            <div className="flex min-h-[360px] flex-col items-center justify-center gap-3 p-8 text-center text-sm text-muted-foreground">
              <Cable className="size-8" />
              选择或添加服务器
            </div>
          )}
        </section>
      </div>

      <section className="rounded-lg border bg-card">
        <div className="flex h-11 items-center justify-between border-b px-3">
          <span className="text-sm font-medium">工具预览</span>
          <Badge variant="outline">{tools.length}</Badge>
        </div>
        {tools.length === 0 ? (
          <div className="px-3 py-6 text-sm text-muted-foreground">
            测试后显示工具
          </div>
        ) : (
          <div className="divide-y">
            {tools.map((tool) => (
              <div key={tool.name} className="grid gap-1 px-3 py-2 text-sm md:grid-cols-[220px_1fr_auto]">
                <code className="min-w-0 truncate font-mono">{tool.name}</code>
                <span className="min-w-0 truncate text-muted-foreground">
                  {tool.description || "无描述"}
                </span>
                <Badge variant="outline">
                  {Object.keys(tool.inputSchema || {}).length} schema keys
                </Badge>
              </div>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}

function Field({
  label,
  children,
  className,
}: {
  label: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={`space-y-1.5 ${className || ""}`}>
      <Label className="text-xs text-muted-foreground">{label}</Label>
      {children}
    </div>
  );
}
