"use client";

import { useState } from "react";
import { Loader2, Copy, Check } from "lucide-react";
import { connectAgentOpenIM } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";

export function OpenIMConnectDialog({ open, onOpenChange, agentId, onConnected }: {
  open: boolean; onOpenChange: (open: boolean) => void; agentId: string; onConnected: () => void;
}) {
  // The parent mounts a fresh form on each opening so credentials do not linger.
  const [apiUrl, setApiUrl] = useState("");
  const [wsUrl, setWsUrl] = useState("");
  const [adminUserId, setAdminUserId] = useState("imAdmin");
  const [adminSecret, setAdminSecret] = useState("");
  const [botUserId, setBotUserId] = useState("");
  const [groups, setGroups] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [webhookUrl, setWebhookUrl] = useState("");
  const [copied, setCopied] = useState(false);

  async function connect(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true); setError("");
    try {
      const result = await connectAgentOpenIM(agentId, {
        apiUrl: apiUrl.trim(), adminUserId: adminUserId.trim(), adminSecret,
        wsUrl: wsUrl.trim(),
        botUserId: botUserId.trim(), allowedGroupIds: groups.split(/[\s,，]+/).filter(Boolean),
      });
      if (!result.ok || !result.webhookUrl) throw new Error(result.error || "连接失败");
      setAdminSecret(""); setWebhookUrl(result.webhookUrl); onConnected();
    } catch (err) { setError(err instanceof Error ? err.message : "连接失败"); }
    finally { setBusy(false); }
  }

  const groupIDs = groups.split(/[\s,，]+/).filter(Boolean);
  const yaml = `url: ${JSON.stringify(webhookUrl)}\nafterSendSingleMsg:\n  enable: true\n  timeout: 5\n  attentionIds: [${JSON.stringify(botUserId.trim())}]\nafterSendGroupMsg:\n  enable: ${groupIDs.length > 0}\n  timeout: 5\n  attentionIds: ${JSON.stringify(groupIDs)}`;

  return <Dialog open={open} onOpenChange={(value) => { if (!busy) onOpenChange(value); }}>
    <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-xl">
      <DialogHeader>
        <DialogTitle>连接 OpenIM</DialogTitle>
        <DialogDescription>将已注册的普通 OpenIM 机器人用户绑定到此智能体，支持单聊和群聊 @ 后的文字回复。</DialogDescription>
      </DialogHeader>
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      {webhookUrl ? <div className="space-y-4">
        <p className="text-sm">凭据已验证并保存。请将以下配置合并到 OpenIM 的 <code>webhooks.yml</code>，并按部署方式重启相关服务以加载配置。</p>
        <pre className="overflow-x-auto rounded-md bg-muted p-3 text-xs whitespace-pre-wrap break-all">{yaml}</pre>
        <Button variant="outline" onClick={async () => {
          try { await navigator.clipboard.writeText(yaml); setCopied(true); }
          catch { setError("复制失败，请手动选择并复制配置。"); }
        }}>{copied ? <Check /> : <Copy />}{copied ? "已复制" : "复制配置"}</Button>
        <p className="text-sm text-muted-foreground">回调地址含访问密钥，请妥善保管。地址必须能从 OpenIM 容器访问；需要时将地址中的域名和端口替换为内部服务地址，保留完整路径。</p>
        <p className="text-sm text-muted-foreground">同一 OpenIM 实例的多个机器人共用此地址；请合并 attentionIds 中的机器人和群 ID，不要覆盖已有列表。机器人需要先加入允许的群。</p>
        <DialogFooter><Button onClick={() => onOpenChange(false)}>完成</Button></DialogFooter>
      </div> : <form onSubmit={connect} className="space-y-4">
        <div className="space-y-2"><Label htmlFor="openim-url">OpenIM API 地址</Label><Input id="openim-url" type="url" required value={apiUrl} onChange={e => setApiUrl(e.target.value)} placeholder="http://openim-server:10002" /><p className="text-xs text-muted-foreground">填写 bkcrab 容器能够访问的 API 地址；同一实例请始终使用相同地址。</p></div>
        <div className="space-y-2"><Label htmlFor="openim-ws">WebSocket 地址（可选）</Label><Input id="openim-ws" type="url" value={wsUrl} onChange={e => setWsUrl(e.target.value)} placeholder="ws://openim-server:10001" /><p className="text-xs text-muted-foreground">用于维持机器人在线状态，需能从 bkcrab 容器访问。留空仍可收发消息，但显示离线。</p></div>
        <div className="space-y-2"><Label htmlFor="openim-admin">管理员用户 ID</Label><Input id="openim-admin" required value={adminUserId} onChange={e => setAdminUserId(e.target.value)} /></div>
        <div className="space-y-2"><Label htmlFor="openim-secret">OpenIM 服务端 secret</Label><Input id="openim-secret" type="password" autoComplete="new-password" required value={adminSecret} onChange={e => setAdminSecret(e.target.value)} /><p className="text-xs text-muted-foreground">用于后端获取管理员 token。同一实例的绑定应使用相同管理员凭据。</p></div>
        <div className="space-y-2"><Label htmlFor="openim-bot">机器人用户 ID</Label><Input id="openim-bot" required value={botUserId} onChange={e => setBotUserId(e.target.value)} placeholder="bkcrab_assistant" /></div>
        <div className="space-y-2"><Label htmlFor="openim-groups">允许的群 ID（可选）</Label><Textarea id="openim-groups" value={groups} onChange={e => setGroups(e.target.value)} placeholder="每行一个群 ID，或用逗号分隔" /><p className="text-xs text-muted-foreground">留空只接收单聊。群消息必须明确 @ 此机器人，@全体不会触发。</p></div>
        <DialogFooter><Button type="button" variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>取消</Button><Button type="submit" disabled={busy || !agentId}>{busy && <Loader2 className="animate-spin" />}验证并连接</Button></DialogFooter>
      </form>}
    </DialogContent>
  </Dialog>;
}
