// 品牌资源路径已复制到 /public/channels/。与仪表板的渠道页面使用
// 同一套，使侧边栏 / 聊天列表和连接对话框共享统一的视觉标识。
const ASSETS: Record<string, string> = {
  telegram: "/channels/telegram.svg",
  discord: "/channels/discord.svg",
  slack: "/channels/slack.svg",
  line: "/channels/line.png",
  feishu: "/channels/feishu.png",
  wechat: "/channels/wechat.svg",
};

// ChannelIcon 在聊天标题旁渲染各渠道的品牌标识。
// 对 web / 未知渠道返回 null — web 是本 UI 中聊天默认的存在方式，
// 因此在每个 web 会话旁加一个通用地球图标只增加噪声没有信息量。
// IM 行仍显示品牌标识以做区分。
//
// 图片自带颜色；我们不添加 text-* 类。微信原始素材为非正方形
// （50×40）— object-contain 使其在框内留白，因此加了一个小缩放
// 使其视觉上与旁边的正方形图标对齐。
export function ChannelIcon({
  channel,
  className = "size-4 shrink-0",
}: {
  channel?: string;
  className?: string;
}) {
  const src = channel ? ASSETS[channel] : undefined;
  if (!src) return null;
  const extra = channel === "wechat" ? "scale-150" : "";
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={src}
      alt={channel ?? ""}
      className={`${className} object-contain ${extra}`}
    />
  );
}

// channelLabel 返回适合工具提示的人类可读名称。
export function channelLabel(channel?: string): string {
  switch (channel) {
    case "telegram":
      return "Telegram";
    case "wechat":
      return "WeChat";
    case "line":
      return "LINE";
    case "discord":
      return "Discord";
    case "slack":
      return "Slack";
    case "feishu":
      return "Feishu";
    case "web":
    case "":
    case undefined:
      return "Web";
    default:
      return channel;
  }
}
