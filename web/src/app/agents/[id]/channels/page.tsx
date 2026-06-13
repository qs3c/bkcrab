"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
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
import {
  Radio,
  Plus,
  Trash2,
  Send,
  CheckCircle2,
  ExternalLink,
  Loader2,
  QrCode,
} from "lucide-react";
import {
  listAgentChannels,
  connectAgentTelegram,
  connectAgentDiscord,
  connectAgentSlack,
  connectAgentLINE,
  connectAgentFeishu,
  startAgentWeChatLogin,
  pollAgentWeChatLoginStatus,
  disconnectAgentChannel,
  type AgentChannel,
} from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";
import { useAgentName } from "@/hooks/use-agent-name";

// 渠道页面：按智能体配置 IM 机器人绑定。目录中每种渠道类型对应一张卡片——
// 已连接的类型显示机器人信息和断开按钮，未连接的显示连接按钮。
// 后端支持每种类型绑定多个机器人；UI 目前只展示第一个绑定，保持
// 简单心智模型（每种渠道每个智能体一个机器人）。后续添加
// 多机器人管理时，卡片可展开为列表。

const CATALOG: { type: string; label: string; description: string; available: boolean }[] = [
  {
    type: "telegram",
    label: "Telegram",
    description: "连接 Telegram 机器人，将消息转发给此智能体。",
    available: true,
  },
  {
    type: "discord",
    label: "Discord",
    description: "连接 Discord 机器人，可在私信和已加入的服务器中使用。",
    available: true,
  },
  {
    type: "slack",
    label: "Slack",
    description: "通过 Socket 模式连接 Slack 应用（机器人令牌 + 应用令牌）。",
    available: true,
  },
  {
    type: "line",
    label: "LINE",
    description: "通过 Webhook 连接 LINE Messaging API 渠道（渠道访问令牌 + 渠道密钥）。",
    available: true,
  },
  {
    type: "wechat",
    label: "WeChat",
    description: "使用手机微信扫描二维码，将消息转发给此智能体。",
    available: true,
  },
  {
    type: "feishu",
    label: "Feishu",
    description: "通过 Webhook 连接飞书自建应用机器人（应用 ID + 应用密钥）。",
    available: true,
  },
];

export default function AgentChannelsPage() {
  const agentId = useAgentIdFromURL();
  const agentName = useAgentName(agentId);

  const [channels, setChannels] = useState<AgentChannel[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const [telegramOpen, setTelegramOpen] = useState(false);
  const [discordOpen, setDiscordOpen] = useState(false);
  const [slackOpen, setSlackOpen] = useState(false);
  const [lineOpen, setLineOpen] = useState(false);
  const [wechatOpen, setWechatOpen] = useState(false);
  const [feishuOpen, setFeishuOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<AgentChannel | null>(null);

  const refresh = useCallback(() => {
    if (!agentId) return;
    setLoading(true);
    listAgentChannels(agentId)
      .then((list) => setChannels(list))
      .catch((e) => setError(e instanceof Error ? e.message : "加载渠道失败"))
      .finally(() => setLoading(false));
  }, [agentId]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // 每种渠道类型的第一个绑定——UI 目前只支持单机器人，
  // 尽管后端允许绑定多个。如果存在多个（历史数据），
  // 其余的仍在服务端生效，只是在此处隐藏。
  const byType = useMemo(() => {
    const m: Record<string, AgentChannel> = {};
    for (const ch of channels) {
      if (!m[ch.type]) m[ch.type] = ch;
    }
    return m;
  }, [channels]);

  const handleDelete = async () => {
    if (!deleteTarget || !agentId) return;
    const target = deleteTarget;
    setDeleteTarget(null);
    const res = await disconnectAgentChannel(agentId, target.type, target.accountId);
    if (res.error) setError(res.error);
    refresh();
  };

  return (
    <div className="p-6 space-y-6 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <div>
          <div className="flex items-center gap-2">
            <Radio className="size-5 text-muted-foreground" />
            <h2 className="text-2xl font-semibold tracking-tight">渠道</h2>
          </div>
          <p className="text-sm text-muted-foreground mt-1">
            为以下智能体连接即时通讯平台： <strong>{agentName || "此智能体"}</strong>{" "}
            让用户可以通过 Telegram、Discord 等平台与其对话。
          </p>
        </div>
      </div>

      {error && (
        <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-4">
          <p className="text-sm text-destructive">{error}</p>
        </div>
      )}

      {loading ? (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          <Skeleton className="h-40" />
          <Skeleton className="h-40" />
          <Skeleton className="h-40" />
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {CATALOG.map((entry) => {
            const connected = byType[entry.type];
            return connected ? (
              <ConnectedCard
                key={entry.type}
                label={entry.label}
                channel={connected}
                onDelete={() => setDeleteTarget(connected)}
              />
            ) : (
              <CatalogCard
                key={entry.type}
                type={entry.type}
                label={entry.label}
                description={entry.description}
                available={entry.available}
                onConnect={() => {
                  if (entry.type === "telegram") setTelegramOpen(true);
                  else if (entry.type === "discord") setDiscordOpen(true);
                  else if (entry.type === "slack") setSlackOpen(true);
                  else if (entry.type === "line") setLineOpen(true);
                  else if (entry.type === "wechat") setWechatOpen(true);
                  else if (entry.type === "feishu") setFeishuOpen(true);
                }}
              />
            );
          })}
        </div>
      )}

      <ConnectTelegramDialog
        open={telegramOpen}
        onOpenChange={setTelegramOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectDiscordDialog
        open={discordOpen}
        onOpenChange={setDiscordOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectSlackDialog
        open={slackOpen}
        onOpenChange={setSlackOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectLINEDialog
        open={lineOpen}
        onOpenChange={setLineOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectWeChatDialog
        open={wechatOpen}
        onOpenChange={setWechatOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <ConnectFeishuDialog
        open={feishuOpen}
        onOpenChange={setFeishuOpen}
        agentId={agentId}
        onConnected={refresh}
      />

      <AlertDialog open={!!deleteTarget} onOpenChange={(v) => !v && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>断开渠道连接</AlertDialogTitle>
            <AlertDialogDescription>
              断开连接{" "}
              <strong>
                {deleteTarget?.botUsername || deleteTarget?.accountId || deleteTarget?.type}
              </strong>
              ？现有对话历史会保留，但机器人将停止向此智能体转发新消息。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              断开连接
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function CatalogCard({
  type,
  label,
  description,
  available,
  onConnect,
}: {
  type: string;
  label: string;
  description: string;
  available: boolean;
  onConnect: () => void;
}) {
  return (
    <div className="rounded-lg border border-border bg-card p-4 flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <ChannelIcon type={type} />
        <span className="font-medium">{label}</span>
      </div>
      <p className="text-xs text-muted-foreground flex-1">{description}</p>
      <Button
        size="sm"
        variant={available ? "outline" : "ghost"}
        disabled={!available}
        onClick={onConnect}
        className="w-full"
      >
        <Plus className="h-3.5 w-3.5 mr-1.5" />
        {available ? "连接" : "即将推出"}
      </Button>
    </div>
  );
}

function ConnectedCard({
  label,
  channel,
  onDelete,
}: {
  label: string;
  channel: AgentChannel;
  onDelete: () => void;
}) {
  // Telegram 是唯一拥有公开个人主页 URL 模式的提供商
  // （t.me/<username>）；Discord/Slack 仅凭机器人用户名无法
  // 拼出公开链接，因此这些平台只显示纯文本。
  const botLink =
    channel.type === "telegram" && channel.botUsername
      ? `https://t.me/${channel.botUsername}`
      : null;

  return (
    <div className="rounded-lg border border-border bg-card p-4 flex flex-col gap-3">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 min-w-0">
          <ChannelIcon type={channel.type} />
          <span className="font-medium truncate">{label}</span>
        </div>
        {channel.enabled && (
          <span className="inline-flex items-center gap-1 text-xs text-emerald-600 dark:text-emerald-400">
            <CheckCircle2 className="h-3 w-3" />
            已连接
          </span>
        )}
      </div>

      <div className="flex-1 space-y-1.5 min-w-0">
        {channel.botUsername && (
          botLink ? (
            <a
              href={botLink}
              target="_blank"
              rel="noreferrer"
              className="text-xs text-muted-foreground hover:text-foreground inline-flex items-center gap-1 truncate max-w-full"
            >
              @{channel.botUsername}
              <ExternalLink className="h-3 w-3 shrink-0" />
            </a>
          ) : (
            <p className="text-xs text-muted-foreground truncate">
              @{channel.botUsername}
            </p>
          )
        )}
        <code className="text-xs text-muted-foreground/80 font-mono truncate block">
          {channel.botToken}
        </code>
      </div>

      <Button
        size="sm"
        variant="outline"
        onClick={onDelete}
        className="w-full text-destructive hover:text-destructive hover:bg-destructive/5"
      >
        <Trash2 className="h-3.5 w-3.5 mr-1.5" />
        断开连接
      </Button>
    </div>
  );
}

function ChannelIcon({ type }: { type: string }) {
  // 品牌 SVG/PNG 资源位于 /public/channels——复制自
  // workany-web 图标集。尺寸设为 16x16 以匹配所替换的
  // lucide 图标；资源自带品牌色彩，无需 text-* 着色类。
  // 微信尚无专用资源，因此回退到 lucide 的 MessageSquare（翠绿色）。
  const asset: Record<string, string> = {
    telegram: "/channels/telegram.svg",
    discord: "/channels/discord.svg",
    slack: "/channels/slack.svg",
    line: "/channels/line.png",
    feishu: "/channels/feishu.png",
    wechat: "/channels/wechat.svg",
  };
  if (asset[type]) {
    // 微信图标非正方形（50×40）——object-contain 会在
    // 16×16 框内留出上下间隙。仅对此图标放大 1.5 倍，
    // 使其视觉重量与相邻的正方形品牌图标一致。
    const extra = type === "wechat" ? "scale-150" : "";
    return (
      <img
        src={asset[type]}
        alt={type}
        className={`h-4 w-4 object-contain ${extra}`}
      />
    );
  }
  return <Radio className="h-4 w-4 text-muted-foreground" />;
}

function ConnectTelegramDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const [token, setToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{ botUsername: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setToken("");
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!token.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentTelegram(agentId, token.trim());
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || "连接失败");
      return;
    }
    setConnected({ botUsername: res.botUsername || "" });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/telegram.svg" alt="Telegram" className="h-5 w-5 object-contain" />
            连接 Telegram 机器人
          </DialogTitle>
          <DialogDescription>
            与以下对象对话：{" "}
            <a
              href="https://t.me/BotFather"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              @BotFather
            </a>{" "}
            在 Telegram 中运行 <code>/newbot</code>，并粘贴返回的 HTTP API
            令牌。系统会通过 <code>getMe</code> 验证令牌，验证通过后再保存配置。
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-emerald-500" />
              <span className="text-sm font-medium">已连接</span>
            </div>
            <p className="text-sm">
              机器人已上线：{" "}
              <a
                href={`https://t.me/${connected.botUsername}`}
                target="_blank"
                rel="noreferrer"
                className="font-mono text-sky-600 dark:text-sky-400 hover:underline inline-flex items-center gap-1"
              >
                @{connected.botUsername}
                <ExternalLink className="h-3 w-3" />
              </a>
              . 在 Telegram 中向机器人发送消息以测试连接。
            </p>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="bot-token">机器人令牌</Label>
              <Input
                id="bot-token"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="123456789:ABCdef..."
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            {error && (
              <p className="text-xs text-destructive">{error}</p>
            )}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>完成</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                取消
              </Button>
              <Button onClick={submit} disabled={submitting || !token.trim()}>
                {submitting ? "正在连接…" : "连接"}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ConnectDiscordDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const [token, setToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{ botUsername: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setToken("");
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!token.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentDiscord(agentId, token.trim());
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || "连接失败");
      return;
    }
    setConnected({ botUsername: res.botUsername || "" });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/discord.svg" alt="Discord" className="h-5 w-5 object-contain" />
            连接 Discord 机器人
          </DialogTitle>
          <DialogDescription>
            打开{" "}
            <a
              href="https://discord.com/developers/applications"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              Discord 开发者门户
            </a>
            ，创建应用、添加机器人并复制机器人令牌。请确保 <strong>消息内容意图</strong> 已在“Bot → Privileged Gateway Intents”下启用。系统会通过{" "}
            <code>/users/@me</code> 保存前会先完成验证。
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-emerald-500" />
              <span className="text-sm font-medium">已连接</span>
            </div>
            <p className="text-sm">
              机器人已上线：{" "}
              <span className="font-mono">{connected.botUsername}</span>。可通过“OAuth2 → URL Generator → Bot scope”将机器人邀请到服务器，或在 Discord 中私信测试。
            </p>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="discord-bot-token">机器人令牌</Label>
              <Input
                id="discord-bot-token"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="MTEx..."
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>完成</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                取消
              </Button>
              <Button onClick={submit} disabled={submitting || !token.trim()}>
                {submitting ? "正在连接…" : "连接"}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ConnectSlackDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const [botToken, setBotToken] = useState("");
  const [appToken, setAppToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{ teamName: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setBotToken("");
      setAppToken("");
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!botToken.trim() || !appToken.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentSlack(agentId, botToken.trim(), appToken.trim());
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || "连接失败");
      return;
    }
    setConnected({ teamName: res.teamName || "" });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/slack.svg" alt="Slack" className="h-5 w-5 object-contain" />
            连接 Slack 应用
          </DialogTitle>
          <DialogDescription>
            在以下地址创建 Slack 应用：{" "}
            <a
              href="https://api.slack.com/apps"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              api.slack.com/apps
            </a>
            。启用 <strong>Socket 模式</strong>，生成一个{" "}
            <strong>应用级令牌</strong> （xapp-…），并授予{" "}
            <code>connections:write</code>，然后在{" "}
            <strong>OAuth 与权限</strong> 复制{" "}
            <strong>机器人用户 OAuth 令牌</strong> （xoxb-…）。然后前往{" "}
            <strong>事件订阅 → 订阅机器人事件</strong> 并添加 <code>message.channels</code>, <code>message.im</code>，并{" "}
            <code>app_mention</code> （Slack 会提示添加对应权限范围： <code>channels:history</code>, <code>im:history</code>,{" "}
            <code>app_mentions:read</code> ，并提示你重新安装）。
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-emerald-500" />
              <span className="text-sm font-medium">已连接</span>
            </div>
            <p className="text-sm">
              机器人已在工作区上线{" "}
              <strong>{connected.teamName}</strong>。使用以下命令将机器人邀请到频道： <code>/invite @bot</code> ，然后发送消息进行测试。
            </p>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="slack-bot-token">机器人用户 OAuth 令牌</Label>
              <Input
                id="slack-bot-token"
                value={botToken}
                onChange={(e) => setBotToken(e.target.value)}
                placeholder="xoxb-..."
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="slack-app-token">应用级令牌</Label>
              <Input
                id="slack-app-token"
                value={appToken}
                onChange={(e) => setAppToken(e.target.value)}
                placeholder="xapp-..."
                className="font-mono text-sm"
              />
            </div>
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>完成</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                取消
              </Button>
              <Button
                onClick={submit}
                disabled={submitting || !botToken.trim() || !appToken.trim()}
              >
                {submitting ? "正在连接…" : "连接"}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// LINE Messaging API 连接对话框。两步式 UX，与飞书一致：
//   1. 用户粘贴渠道访问令牌 + 渠道密钥；调用 /v2/bot/info 验证
//      并获取机器人的 userId。
//   2. 成功后展示公开 Webhook URL——用户将其粘贴到 LINE 开发者
//      控制台的"消息 API → Webhook 地址"下，并开启"使用 Webhook"。
function ConnectLINEDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const [channelToken, setChannelToken] = useState("");
  const [channelSecret, setChannelSecret] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{ botName: string; basicId: string; webhookUrl: string } | null>(null);

  useEffect(() => {
    if (!open) {
      setChannelToken("");
      setChannelSecret("");
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!channelToken.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentLINE(
      agentId,
      channelToken.trim(),
      channelSecret.trim(),
    );
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || "连接失败");
      return;
    }
    setConnected({
      botName: res.botName || "",
      basicId: res.basicId || "",
      webhookUrl: res.webhookUrl || "",
    });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/line.png" alt="LINE" className="h-5 w-5 object-contain" />
            连接 LINE 渠道
          </DialogTitle>
          <DialogDescription>
            在以下地址创建 Messaging API 渠道：{" "}
            <a
              href="https://developers.line.biz"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              developers.line.biz
            </a>
            。在 <strong>消息 API</strong> 签发长期有效的{" "}
            <strong>渠道访问令牌</strong>，并复制{" "}
            <strong>渠道密钥</strong> 。该值可从“基本设置”标签页复制。保存我们生成的 URL 后，请开启 <em>使用 Webhook</em> ?
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="space-y-3 py-2">
            <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 space-y-2">
              <div className="flex items-center gap-2">
                <CheckCircle2 className="h-4 w-4 text-emerald-500" />
                <span className="text-sm font-medium">凭据有效</span>
              </div>
              <p className="text-sm">
                已识别机器人：{" "}
                <strong>{connected.botName || "（未命名）"}</strong>{" "}
                {connected.basicId && (
                  <code className="font-mono text-xs">{connected.basicId}</code>
                )}
                。
              </p>
            </div>
            <div className="rounded-lg border bg-muted/30 p-4 space-y-2">
              <p className="text-sm font-medium">最后一步</p>
              <p className="text-xs text-muted-foreground">
                将此内容粘贴到 LINE 开发者控制台 →{" "}
                <strong>消息 API → Webhook 地址</strong>，点击{" "}
                <em>验证</em>，然后开启
                <strong>使用 Webhook</strong>。
              </p>
              <Input
                readOnly
                value={connected.webhookUrl}
                className="font-mono text-xs"
                onFocus={(e) => e.currentTarget.select()}
              />
              <p className="text-xs text-muted-foreground">
                搜索基础 ID 将机器人添加为好友，或邀请进群，然后发送消息进行测试。
              </p>
            </div>
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="space-y-1.5">
              <Label htmlFor="line-channel-token">渠道访问令牌</Label>
              <Input
                id="line-channel-token"
                value={channelToken}
                onChange={(e) => setChannelToken(e.target.value)}
                placeholder="长期有效令牌"
                type="password"
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="line-channel-secret">渠道密钥</Label>
              <Input
                id="line-channel-secret"
                value={channelSecret}
                onChange={(e) => setChannelSecret(e.target.value)}
                placeholder="，可从“基本设置”复制"
                className="font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                可选但强烈建议配置。bkclaw 会使用此密钥通过 HMAC-SHA256 验证传入的 Webhook 请求。
              </p>
            </div>
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>完成</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                取消
              </Button>
              <Button
                onClick={submit}
                disabled={submitting || !channelToken.trim()}
              >
                {submitting ? "正在验证…" : "连接"}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ConnectWeChatDialog 驱动扫码登录流程：获取会话令牌，
// 将其 qrCode 字符串渲染为二维码，然后每 3 秒轮询服务器状态。
// 轮询端点每次调用只做一次上游往返（我们不做长轮询），
// 因此生命周期完全由客户端驱动——关闭对话框时通过轮询
// 引用清理资源。
function ConnectWeChatDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  type WechatStatus = "wait" | "scaned" | "confirmed" | "expired" | "";
  const [qrPayload, setQrPayload] = useState("");
  const [sessionId, setSessionId] = useState("");
  const [status, setStatus] = useState<WechatStatus>("");
  const [accountId, setAccountId] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  // 组件卸载和对话框关闭时清理轮询。
  useEffect(() => () => stopPolling(), [stopPolling]);
  useEffect(() => {
    if (!open) {
      stopPolling();
      setQrPayload("");
      setSessionId("");
      setStatus("");
      setAccountId("");
      setError("");
      setLoading(false);
    }
  }, [open, stopPolling]);

  const startLogin = useCallback(async () => {
    if (!agentId) return;
    setLoading(true);
    setError("");
    setStatus("");
    setAccountId("");
    setQrPayload("");
    stopPolling();
    const res = await startAgentWeChatLogin(agentId);
    setLoading(false);
    if (res.error || !res.sessionId || !res.qrCodeImg) {
      setError(res.error || "获取二维码失败");
      return;
    }
    setSessionId(res.sessionId);
    setQrPayload(res.qrCodeImg);
    setStatus("wait");
    pollRef.current = setInterval(async () => {
      const s = await pollAgentWeChatLoginStatus(agentId, res.sessionId!);
      if (s.error) {
        // 不要因单次瞬态错误终止轮询——iLink 的状态端点偶有波动，
        // 下一次轮询通常能恢复。仅以横幅形式展示错误。
        setError(s.error);
        return;
      }
      setError("");
      if (s.status) setStatus(s.status as WechatStatus);
      if (s.connected) {
        stopPolling();
        if (s.accountId) setAccountId(s.accountId);
        onConnected();
      }
      if (s.status === "expired") {
        stopPolling();
      }
    }, 3000);
  }, [agentId, onConnected, stopPolling]);

  // 对话框打开时自动获取二维码（无需单独的"命名"步骤——
  // bkclaw 不展示每账户名称，accountId 即 ilink_bot_id）。
  useEffect(() => {
    if (open && !qrPayload && !loading && !error) {
      startLogin();
    }
  }, [open, qrPayload, loading, error, startLogin]);

  const connected = !!accountId;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-[420px]">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/wechat.svg" alt="WeChat" className="h-5 w-5 object-contain scale-150" />
            连接微信
          </DialogTitle>
          <DialogDescription>
            使用手机微信扫描二维码，将个人微信账户绑定为此智能体的机器人。收到的私信会转发给智能体，智能体的回复将以纯文本发回。
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 space-y-2">
            <div className="flex items-center gap-2">
              <CheckCircle2 className="h-4 w-4 text-emerald-500" />
              <span className="text-sm font-medium">已连接</span>
            </div>
            <p className="text-sm">
              机器人已上线： <code className="font-mono text-xs">{accountId}</code>.
              向微信机器人发送消息以测试连接。
            </p>
          </div>
        ) : (
          <div className="flex flex-col items-center gap-4 py-2">
            {loading ? (
              <div className="flex h-56 w-56 items-center justify-center">
                <Loader2 className="h-8 w-8 animate-spin text-muted-foreground" />
              </div>
            ) : qrPayload ? (
              <div className="rounded-lg border bg-white p-4">
                <QRCodeSVG value={qrPayload} size={224} level="M" />
              </div>
            ) : (
              <div className="flex h-56 w-56 items-center justify-center text-sm text-muted-foreground">
                <QrCode className="h-8 w-8 opacity-50" />
              </div>
            )}

            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              {status === "wait" && <>等待扫码…</>}
              {status === "scaned" && (
                <>
                  <CheckCircle2 className="h-4 w-4 text-emerald-500" />
                  已扫码，请在手机上确认。
                </>
              )}
              {status === "confirmed" && (
                <>
                  <Loader2 className="h-4 w-4 animate-spin" />
                  正在连接…
                </>
              )}
              {status === "expired" && (
                <span className="text-destructive">二维码已过期。</span>
              )}
            </div>

            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>完成</Button>
          ) : (
            <>
              {status === "expired" && (
                <Button onClick={startLogin} disabled={loading}>
                  {loading ? "正在刷新…" : "刷新二维码"}
                </Button>
              )}
              <Button variant="outline" onClick={() => onOpenChange(false)}>
                取消
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// 飞书连接对话框。两步式 UX：
//   1. 用户粘贴应用 ID + 应用密钥 + 验证令牌，通过
//      /tenant_access_token + /bot/v3/info 验证。
//   2. 成功后展示 Webhook URL——用户需将其粘贴到飞书开发者控制台
//      的"事件订阅 → 请求地址"下，并在此处触发飞书的 URL 验证
//      握手，机器人才能开始接收消息。
function ConnectFeishuDialog({
  open,
  onOpenChange,
  agentId,
  onConnected,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  agentId: string;
  onConnected: () => void;
}) {
  const [appId, setAppId] = useState("");
  const [appSecret, setAppSecret] = useState("");
  const [verificationToken, setVerificationToken] = useState("");
  const [encryptKey, setEncryptKey] = useState("");
  const [useLongConn, setUseLongConn] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [connected, setConnected] = useState<{
    botName: string;
    webhookUrl: string;
    useLongConn: boolean;
  } | null>(null);

  useEffect(() => {
    if (!open) {
      setAppId("");
      setAppSecret("");
      setVerificationToken("");
      setEncryptKey("");
      setUseLongConn(true);
      setError("");
      setSubmitting(false);
      setConnected(null);
    }
  }, [open]);

  const submit = async () => {
    if (!appId.trim() || !appSecret.trim() || !agentId) return;
    setSubmitting(true);
    setError("");
    const res = await connectAgentFeishu(
      agentId,
      appId.trim(),
      appSecret.trim(),
      verificationToken.trim(),
      encryptKey.trim(),
      useLongConn,
    );
    setSubmitting(false);
    if (res.error || !res.ok) {
      setError(res.error || "连接失败");
      return;
    }
    setConnected({
      botName: res.botName || "",
      webhookUrl: res.webhookUrl || "",
      useLongConn: !!res.useLongConn,
    });
    onConnected();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <img src="/channels/feishu.png" alt="Feishu" className="h-5 w-5 object-contain" />
            连接飞书应用
          </DialogTitle>
          <DialogDescription>
            在以下地址创建自定义应用：{" "}
            <a
              href="https://open.feishu.cn"
              target="_blank"
              rel="noreferrer"
              className="underline"
            >
              open.feishu.cn
            </a>
            。启用机器人能力并申请{" "}
            <code>im:message</code> + <code>im:message:send_as_bot</code>{" "}
            权限，然后从以下位置复制应用 ID 和应用密钥：{" "}
            <strong>凭据与基本信息</strong>。长连接模式（推荐）无需其他配置；Webhook 模式还需要从以下位置获取验证令牌和加密密钥：{" "}
            <strong>事件订阅</strong>。
          </DialogDescription>
        </DialogHeader>

        {connected ? (
          <div className="space-y-3 py-2">
            <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 space-y-2">
              <div className="flex items-center gap-2">
                <CheckCircle2 className="h-4 w-4 text-emerald-500" />
                <span className="text-sm font-medium">凭据有效</span>
              </div>
              <p className="text-sm">
                已识别机器人：{" "}
                <strong>{connected.botName || "（未命名）"}</strong>。
              </p>
            </div>
            {connected.useLongConn ? (
              <div className="rounded-lg border bg-muted/30 p-4 space-y-2">
                <p className="text-sm font-medium">长连接模式</p>
                <p className="text-xs text-muted-foreground">
                  bkclaw 正在通过 WebSocket 连接飞书，无需配置公网 URL。请在飞书开发者后台的{" "}
                  <strong>事件与回调 → 事件配置 → 订阅方式</strong>，选择{" "}
                  <strong>使用长连接接收事件</strong>，然后在{" "}
                  <strong>订阅机器人事件</strong> 添加{" "}
                  <code>im.message.receive_v1</code>.
                </p>
              </div>
            ) : (
              <div className="rounded-lg border bg-muted/30 p-4 space-y-2">
                <p className="text-sm font-medium">最后一步</p>
                <p className="text-xs text-muted-foreground">
                  将此内容粘贴到飞书开发者后台 →{" "}
                  <strong>事件订阅 → 请求地址</strong>，然后点击 <em>保存</em>
                  。飞书会向此处发送 POST 验证请求，bkclaw 会自动返回验证内容。
                </p>
                <Input
                  readOnly
                  value={connected.webhookUrl}
                  className="font-mono text-xs"
                  onFocus={(e) => e.currentTarget.select()}
                />
                <p className="text-xs text-muted-foreground">
                  订阅 <code>im.message.receive_v1</code> 以接收消息。
                </p>
              </div>
            )}
          </div>
        ) : (
          <div className="space-y-3 py-2">
            <div className="flex items-start justify-between gap-3 rounded-lg border bg-muted/30 p-3">
              <div className="space-y-0.5">
                <Label htmlFor="feishu-long-conn" className="text-sm">
                  长连接模式
                </Label>
                <p className="text-xs text-muted-foreground">
                  bkclaw 将通过 WebSocket 连接飞书，无需公网 URL。关闭后可使用传统 Webhook 流程。
                </p>
              </div>
              <Switch
                id="feishu-long-conn"
                checked={useLongConn}
                onCheckedChange={setUseLongConn}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="feishu-app-id">应用 ID</Label>
              <Input
                id="feishu-app-id"
                value={appId}
                onChange={(e) => setAppId(e.target.value)}
                placeholder="cli_..."
                className="font-mono text-sm"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="feishu-app-secret">应用密钥</Label>
              <Input
                id="feishu-app-secret"
                value={appSecret}
                onChange={(e) => setAppSecret(e.target.value)}
                placeholder="..."
                type="password"
                className="font-mono text-sm"
              />
            </div>
            {!useLongConn && (
              <>
            <div className="space-y-1.5">
              <Label htmlFor="feishu-verification-token">验证令牌</Label>
              <Input
                id="feishu-verification-token"
                value={verificationToken}
                onChange={(e) => setVerificationToken(e.target.value)}
                placeholder="，可从“事件订阅”标签页获取"
                className="font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                可选但建议配置。bkclaw 会拒绝 <code>header.token</code> 不匹配的 Webhook
                请求。
              </p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="feishu-encrypt-key">加密密钥</Label>
              <Input
                id="feishu-encrypt-key"
                value={encryptKey}
                onChange={(e) => setEncryptKey(e.target.value)}
                placeholder="未配置加密策略时请留空"
                type="password"
                className="font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                仅在飞书控制台的 <strong>加密策略</strong>
                中设置了加密密钥时需要。留空表示接收明文 Webhook 请求体。
              </p>
            </div>
              </>
            )}
            {error && <p className="text-xs text-destructive">{error}</p>}
          </div>
        )}

        <DialogFooter>
          {connected ? (
            <Button onClick={() => onOpenChange(false)}>完成</Button>
          ) : (
            <>
              <Button
                variant="outline"
                onClick={() => onOpenChange(false)}
                disabled={submitting}
              >
                取消
              </Button>
              <Button
                onClick={submit}
                disabled={submitting || !appId.trim() || !appSecret.trim()}
              >
                {submitting ? "正在验证…" : "连接"}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
