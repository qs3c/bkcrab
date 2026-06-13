"use client";

import { useState, useCallback, useEffect } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Switch } from "@/components/ui/switch";
import { Separator } from "@/components/ui/separator";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  ArrowLeft,
  ArrowRight,
  Bot,
  Check,
  Container,
  KeyRound,
  Loader2,
  PartyPopper,
  Sparkles,
  UserPlus,
} from "lucide-react";
import { getStatus, onboard, testProvider } from "@/lib/api";

const STEPS = [
  { id: "welcome", label: "Welcome", icon: PartyPopper },
  { id: "admin", label: "管理员", icon: UserPlus },
  { id: "provider", label: "服务商", icon: KeyRound },
  { id: "agent", label: "智能体", icon: Bot },
  { id: "sandbox", label: "沙箱", icon: Container },
  { id: "launch", label: "Launch", icon: Sparkles },
] as const;

// 显示标签映射。base-ui 的 <Select.Value /> 默认渲染原始
// `value`（SelectItem 的 value 属性），而非 SelectItem 的子元素——
// 因此我们通过 SelectValue 的 children 渲染属性显式映射键到标题。
// 保持以下映射与 SelectItem 列表同步。
const PROVIDER_LABELS: Record<string, string> = {
  openai: "OpenAI",
  openrouter: "OpenRouter",
  anthropic: "Anthropic",
  deepseek: "DeepSeek",
  ollama: "Ollama",
  custom: "自定义",
};

const API_TYPE_LABELS: Record<string, string> = {
  "openai-chat": "OpenAI 聊天补全",
  "anthropic-messages": "Anthropic 消息",
};

const AUTH_TYPE_LABELS: Record<string, string> = {
  "bearer-token": "Bearer 令牌",
  "api-key": "API 密钥请求头",
};

// PROVIDERS 保存用户从下拉列表选择服务商时表单预填的每预设默认值。
// `models[0]` 作为默认模型输入的占位符显示——用户可以覆盖输入。
// authType 也会同步，避免从 Anthropic（api-key）切换到
// Bearer-token 服务商时表单停留在错误的认证方式上。
const PROVIDERS: Record<
  string,
  { apiBase: string; apiType: string; authType: string; models: string[] }
> = {
  openai: {
    apiBase: "https://api.openai.com/v1",
    apiType: "openai-chat",
    authType: "bearer-token",
    models: ["gpt-5.5"],
  },
  openrouter: {
    apiBase: "https://openrouter.ai/api/v1",
    apiType: "openai-chat",
    authType: "bearer-token",
    models: ["google/gemini-3-flash-preview"],
  },
  anthropic: {
    apiBase: "https://api.anthropic.com",
    apiType: "anthropic-messages",
    authType: "api-key",
    models: ["claude-opus-4.7", "claude-sonnet-4.7", "claude-haiku-4.5"],
  },
  deepseek: {
    apiBase: "https://api.deepseek.com",
    apiType: "openai-chat",
    authType: "bearer-token",
    models: ["deepseek-v4-pro"],
  },
  ollama: {
    apiBase: "http://localhost:11434/v1",
    apiType: "openai-chat",
    authType: "bearer-token",
    models: ["qwen3.5:35b-a3b-int4"],
  },
  custom: { apiBase: "", apiType: "openai-chat", authType: "bearer-token", models: [] },
};

export default function OnboardPage() {
  const router = useRouter();
  const [step, setStep] = useState(0);

  // 已完成初始化检测——/api/status 在存在任何账户时返回
  // configured=true，此情况下向导无需操作，直接将访客
  // 跳转到仪表盘。使用 router.replace 进行重定向，
  // 避免后退按钮将用户带回初始化页。
  useEffect(() => {
    let cancelled = false;
    getStatus()
      .then((s) => {
        if (!cancelled && s?.configured) router.replace("/overview/");
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [router]);

  // 管理员
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [passwordConfirm, setPasswordConfirm] = useState("");
  const [displayName, setDisplayName] = useState("");

  // 服务商
  const [providerEnabled, setProviderEnabled] = useState(true);
  const [providerKey, setProviderKey] = useState("openai");
  const [providerName, setProviderName] = useState("openai");
  const [apiBase, setApiBase] = useState(PROVIDERS.openai.apiBase);
  const [apiKey, setApiKey] = useState("");
  const [apiType, setApiType] = useState(PROVIDERS.openai.apiType);
  const [authType, setAuthType] = useState("bearer-token");
  const [model, setModel] = useState(PROVIDERS.openai.models[0]);
  const [testStatus, setTestStatus] = useState<"" | "ok" | "fail" | "running">(
    "",
  );
  const [testError, setTestError] = useState("");

  // 智能体
  const [agentName, setAgentName] = useState("default");

  // 沙箱（可选——默认关闭；用户可开启并配置）
  const [sandboxEnabled, setSandboxEnabled] = useState(false);
  const [sandboxBackend, setSandboxBackend] = useState("docker");
  const [sandboxDockerImage, setSandboxDockerImage] = useState("thinkany/bkclaw-sandbox:latest");
  const [sandboxE2BTemplate, setSandboxE2BTemplate] = useState("base");
  const [sandboxE2BKey, setSandboxE2BKey] = useState("");
  const [sandboxBoxliteImage, setSandboxBoxliteImage] = useState("");
  const [sandboxBoxliteKey, setSandboxBoxliteKey] = useState("");
  const [sandboxBoxliteURL, setSandboxBoxliteURL] = useState("");

  // 提交状态
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState("");

  const handleProviderChange = useCallback((next: string) => {
    setProviderKey(next);
    const preset = PROVIDERS[next];
    if (preset) {
      setApiBase(preset.apiBase);
      setApiType(preset.apiType);
      setAuthType(preset.authType);
      if (preset.models[0]) setModel(preset.models[0]);
    }
    // 服务商名称自动填充为预设键——用户仍可覆盖
    // （如将 "openai" 重命名为 "production"）。
    // 自定义服务商清空字段让用户自行输入。
    setProviderName(next === "custom" ? "" : next);
    setTestStatus("");
    setTestError("");
  }, []);

  async function handleTest() {
    if (!apiKey) {
      setTestStatus("fail");
      setTestError("API key required");
      return;
    }
    setTestStatus("running");
    setTestError("");
    const res = await testProvider({ apiBase, apiKey, model, apiType, authType });
    if (res.ok) {
      setTestStatus("ok");
    } else {
      setTestStatus("fail");
      setTestError(res.error || "测试失败");
    }
  }

  async function handleSubmit() {
    setSubmitError("");
    setSubmitting(true);
    // 用户可以重命名预设服务商；我们仍会对输入进行 slug 化
    // （小写、连字符），使其成为数据库中的整洁键。
    // 当 providerEnabled 为 false 时，发送空的服务商/apiKey/model，
    // 使后端的 `if req.Provider != "" && req.APIKey != ""` 守卫完全
    // 跳过服务商+默认值写入（handlers_admin.go:240）。
    const finalProviderName =
      providerName.trim().toLowerCase().replace(/\s+/g, "-") || providerKey;
    const res = await onboard({
      username,
      email,
      password,
      displayName,
      provider: providerEnabled ? finalProviderName : "",
      apiBase: providerEnabled ? apiBase : "",
      apiKey: providerEnabled ? apiKey : "",
      apiType: providerEnabled ? apiType : "",
      authType: providerEnabled ? authType : "",
      model: providerEnabled ? model : "",
      agentName,
      sandboxEnabled,
      sandboxBackend: sandboxEnabled ? sandboxBackend : undefined,
      sandboxImage: sandboxEnabled
        ? sandboxBackend === "docker"
          ? sandboxDockerImage
          : sandboxBackend === "e2b"
            ? sandboxE2BTemplate
            : sandboxBackend === "boxlite"
              ? sandboxBoxliteImage
              : undefined
        : undefined,
      sandboxE2BKey: sandboxEnabled && sandboxBackend === "e2b" ? sandboxE2BKey : undefined,
      sandboxBoxliteKey: sandboxEnabled && sandboxBackend === "boxlite" ? sandboxBoxliteKey : undefined,
      sandboxBoxliteUrl:
        sandboxEnabled && sandboxBackend === "boxlite" && sandboxBoxliteURL
          ? sandboxBoxliteURL
          : undefined,
    });
    setSubmitting(false);
    if (!res.ok) {
      setSubmitError(res.error || "初始化失败");
      setStep(1); // 跳回管理员步骤，大多数错误来自此处
      return;
    }
    setStep(STEPS.length - 1);
  }

  // 每步的验证——驱动"下一步"按钮的禁用状态。
  const sandboxValid =
    !sandboxEnabled ||
    (sandboxBackend === "docker"
      ? sandboxDockerImage.trim() !== ""
      : sandboxBackend === "e2b"
        ? sandboxE2BKey.trim() !== "" && sandboxE2BTemplate.trim() !== ""
        : sandboxBackend === "boxlite"
          ? sandboxBoxliteKey.trim() !== "" && sandboxBoxliteImage.trim() !== ""
          : false);
  const stepValid: boolean[] = [
    true,
    username.trim() !== "" &&
      email.trim() !== "" &&
      password.length >= 6 &&
      password === passwordConfirm,
    !providerEnabled ||
      (apiKey.trim() !== "" && model.trim() !== "" && apiBase.trim() !== "" && testStatus === "ok"),
    agentName.trim() !== "",
    sandboxValid,
    true,
  ];

  return (
    <div className="flex min-h-screen items-center justify-center bg-muted/30 p-4">
      <div className="w-full max-w-2xl space-y-6">
        <Stepper current={step} />

        {step === 0 && <WelcomeStep />}

        {step === 1 && (
          <AdminStep
            username={username}
            setUsername={setUsername}
            email={email}
            setEmail={setEmail}
            password={password}
            setPassword={setPassword}
            passwordConfirm={passwordConfirm}
            setPasswordConfirm={setPasswordConfirm}
            displayName={displayName}
            setDisplayName={setDisplayName}
          />
        )}

        {step === 2 && (
          <ProviderStep
            enabled={providerEnabled}
            setEnabled={setProviderEnabled}
            providerKey={providerKey}
            onProviderChange={handleProviderChange}
            providerName={providerName}
            setProviderName={setProviderName}
            apiBase={apiBase}
            setApiBase={setApiBase}
            apiKey={apiKey}
            setApiKey={setApiKey}
            apiType={apiType}
            setApiType={setApiType}
            authType={authType}
            setAuthType={setAuthType}
            model={model}
            setModel={setModel}
            onTest={handleTest}
            testStatus={testStatus}
            testError={testError}
          />
        )}

        {step === 3 && (
          <AgentStep agentName={agentName} setAgentName={setAgentName} />
        )}

        {step === 4 && (
          <SandboxStep
            enabled={sandboxEnabled}
            setEnabled={setSandboxEnabled}
            backend={sandboxBackend}
            setBackend={setSandboxBackend}
            dockerImage={sandboxDockerImage}
            setDockerImage={setSandboxDockerImage}
            e2bTemplate={sandboxE2BTemplate}
            setE2BTemplate={setSandboxE2BTemplate}
            e2bKey={sandboxE2BKey}
            setE2BKey={setSandboxE2BKey}
            boxliteImage={sandboxBoxliteImage}
            setBoxliteImage={setSandboxBoxliteImage}
            boxliteKey={sandboxBoxliteKey}
            setBoxliteKey={setSandboxBoxliteKey}
            boxliteURL={sandboxBoxliteURL}
            setBoxliteURL={setSandboxBoxliteURL}
          />
        )}

        {step === 5 && <DoneStep onContinue={() => router.replace("/")} />}

        {submitError && (
          <Card className="border-destructive/40 bg-destructive/5">
            <CardContent className="pt-6">
              <p className="text-sm text-destructive">{submitError}</p>
            </CardContent>
          </Card>
        )}

        {step !== STEPS.length - 1 && (
          <div className="flex items-center justify-between">
            <Button
              variant="ghost"
              onClick={() => setStep((s) => Math.max(0, s - 1))}
              disabled={step === 0}
            >
              <ArrowLeft className="mr-1 size-4" /> 返回
            </Button>
            {step < STEPS.length - 2 ? (
              <Button
                onClick={() => setStep((s) => s + 1)}
                disabled={!stepValid[step]}
              >
                下一步 <ArrowRight className="ml-1 size-4" />
              </Button>
            ) : (
              <Button
                onClick={handleSubmit}
                disabled={!stepValid[step] || submitting}
              >
                {submitting ? (
                  <>
                    <Loader2 className="mr-1 size-4 animate-spin" /> 正在设置
                  </>
                ) : (
                  <>
                    创建并启动 <Sparkles className="ml-1 size-4" />
                  </>
                )}
              </Button>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function Stepper({ current }: { current: number }) {
  return (
    <ol className="flex items-center gap-2">
      {STEPS.map((s, i) => {
        const Icon = s.icon;
        const done = i < current;
        const active = i === current;
        return (
          <li key={s.id} className="flex flex-1 items-center gap-2">
            <div
              className={
                "flex size-8 shrink-0 items-center justify-center rounded-full border transition " +
                (done
                  ? "border-primary bg-primary text-primary-foreground"
                  : active
                    ? "border-primary text-primary"
                    : "border-border text-muted-foreground")
              }
            >
              {done ? <Check className="size-4" /> : <Icon className="size-4" />}
            </div>
            <span
              className={
                "hidden text-sm sm:inline " +
                (active
                  ? "font-medium"
                  : done
                    ? "text-muted-foreground"
                    : "text-muted-foreground/60")
              }
            >
              {s.label}
            </span>
            {i < STEPS.length - 1 && (
              <div
                className={
                  "h-px flex-1 " +
                  (i < current ? "bg-primary" : "bg-border")
                }
              />
            )}
          </li>
        );
      })}
    </ol>
  );
}

function WelcomeStep() {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <PartyPopper className="size-5 text-primary" />
          欢迎使用 BkClaw
        </CardTitle>
        <CardDescription>
          只需几个步骤即可完成平台设置：管理员账户、第一个大模型服务商和第一个智能体。大约需要一分钟。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3 text-sm text-muted-foreground">
        <p>设置完成后，你将成为超级管理员，之后可在管理后台添加更多用户。</p>
        <p>
          所有面向用户的配置（服务商、渠道、智能体、设置）都会保存在数据库中，之后可在界面中修改。
        </p>
      </CardContent>
    </Card>
  );
}

function AdminStep(props: {
  username: string;
  setUsername: (v: string) => void;
  email: string;
  setEmail: (v: string) => void;
  password: string;
  setPassword: (v: string) => void;
  passwordConfirm: string;
  setPasswordConfirm: (v: string) => void;
  displayName: string;
  setDisplayName: (v: string) => void;
}) {
  const passwordTooShort =
    props.password.length > 0 && props.password.length < 6;
  const mismatch =
    props.passwordConfirm.length > 0 && props.password !== props.passwordConfirm;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <UserPlus className="size-5 text-primary" />
          创建超级管理员账户
        </CardTitle>
        <CardDescription>
          之后可使用用户名或邮箱登录。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="ob-username">用户名</Label>
            <Input
              id="ob-username"
              value={props.username}
              onChange={(e) => props.setUsername(e.target.value)}
              autoComplete="username"
              placeholder="alice"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ob-email">邮箱</Label>
            <Input
              id="ob-email"
              type="email"
              value={props.email}
              onChange={(e) => props.setEmail(e.target.value)}
              autoComplete="email"
              placeholder="alice@example.com"
            />
          </div>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="ob-display">显示名称（可选）</Label>
          <Input
            id="ob-display"
            value={props.displayName}
            onChange={(e) => props.setDisplayName(e.target.value)}
            placeholder="Alice"
          />
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="ob-password">密码</Label>
            <Input
              id="ob-password"
              type="password"
              value={props.password}
              onChange={(e) => props.setPassword(e.target.value)}
              autoComplete="new-password"
              placeholder="至少 6 个字符"
            />
            {passwordTooShort && (
              <p className="text-xs text-destructive">至少 6 个字符</p>
            )}
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ob-password2">确认密码</Label>
            <Input
              id="ob-password2"
              type="password"
              value={props.passwordConfirm}
              onChange={(e) => props.setPasswordConfirm(e.target.value)}
              autoComplete="new-password"
            />
            {mismatch && (
              <p className="text-xs text-destructive">两次输入的密码不一致</p>
            )}
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

function ProviderStep(props: {
  enabled: boolean;
  setEnabled: (v: boolean) => void;
  providerKey: string;
  onProviderChange: (v: string) => void;
  providerName: string;
  setProviderName: (v: string) => void;
  apiBase: string;
  setApiBase: (v: string) => void;
  apiKey: string;
  setApiKey: (v: string) => void;
  apiType: string;
  setApiType: (v: string) => void;
  authType: string;
  setAuthType: (v: string) => void;
  model: string;
  setModel: (v: string) => void;
  onTest: () => void;
  testStatus: "" | "ok" | "fail" | "running";
  testError: string;
}) {
  const preset = PROVIDERS[props.providerKey];
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-5 text-primary" />
          第一个大模型服务商
        </CardTitle>
        <CardDescription>
          请至少连接一个模型。之后可在“服务商”页面添加更多模型以及用户级、智能体级覆盖配置，也可以跳过并稍后统一配置。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-center justify-between">
          <div>
            <p className="text-sm font-medium">立即配置服务商</p>
            <p className="text-xs text-muted-foreground">
              关闭即跳过；之后可在“服务商”页面添加。
            </p>
          </div>
          <Switch checked={props.enabled} onCheckedChange={props.setEnabled} />
        </div>
        {props.enabled && <Separator />}
        {!props.enabled && (
          <p className="text-xs text-muted-foreground">
            已跳过。管理员账户和智能体将不配置默认模型，可稍后从以下位置添加：{" "}
            <span className="font-mono">服务商</span> 启动后。
          </p>
        )}
        {props.enabled && (
        <>
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label>服务商</Label>
            <Select
              value={props.providerKey}
              onValueChange={(v) => v && props.onProviderChange(v)}
            >
              <SelectTrigger className="w-full">
                <SelectValue>
                  {(v: unknown) => PROVIDER_LABELS[v as string] ?? (v as string) ?? ""}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="openai">OpenAI</SelectItem>
                <SelectItem value="openrouter">OpenRouter</SelectItem>
                <SelectItem value="anthropic">Anthropic</SelectItem>
                <SelectItem value="deepseek">DeepSeek</SelectItem>
                <SelectItem value="ollama">Ollama</SelectItem>
                <SelectItem value="custom">自定义</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label>服务商名称</Label>
            <Input
              value={props.providerName}
              onChange={(e) => props.setProviderName(e.target.value)}
              placeholder="openai"
              className="font-mono text-sm"
            />
          </div>
        </div>

        <div className="space-y-1.5">
          <Label>默认模型</Label>
          <Input
            value={props.model}
            onChange={(e) => props.setModel(e.target.value)}
            placeholder={preset?.models[0] || "model-id"}
            className="font-mono text-sm"
          />
        </div>
        <div className="space-y-1.5">
          <Label>API 基础地址</Label>
          <Input
            value={props.apiBase}
            onChange={(e) => props.setApiBase(e.target.value)}
            className="font-mono text-sm"
          />
        </div>
        <div className="space-y-1.5">
          <Label>API 密钥</Label>
          <Input
            type="password"
            value={props.apiKey}
            onChange={(e) => props.setApiKey(e.target.value)}
            placeholder="sk-…"
            className="font-mono text-sm"
          />
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label>API 类型</Label>
            <Select value={props.apiType} onValueChange={(v) => v && props.setApiType(v)}>
              <SelectTrigger className="w-full">
                <SelectValue>
                  {(v: unknown) => API_TYPE_LABELS[v as string] ?? (v as string) ?? ""}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="openai-chat">OpenAI 聊天补全</SelectItem>
                <SelectItem value="anthropic-messages">Anthropic 消息</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label>认证类型</Label>
            <Select value={props.authType} onValueChange={(v) => v && props.setAuthType(v)}>
              <SelectTrigger className="w-full">
                <SelectValue>
                  {(v: unknown) => AUTH_TYPE_LABELS[v as string] ?? (v as string) ?? ""}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="bearer-token">Bearer 令牌</SelectItem>
                <SelectItem value="api-key">API 密钥请求头</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>

        <div className="flex items-center gap-3 pt-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={props.onTest}
            disabled={props.testStatus === "running" || !props.apiKey}
          >
            {props.testStatus === "running" ? (
              <>
                <Loader2 className="mr-1 size-4 animate-spin" /> 正在测试
              </>
            ) : (
              "测试连接"
            )}
          </Button>
          {props.testStatus === "ok" && (
            <Badge className="bg-emerald-500/15 text-emerald-700 hover:bg-emerald-500/15">
              <Check className="mr-1 size-3" /> 已连接
            </Badge>
          )}
          {props.testStatus === "fail" && (
            <span className="text-xs text-destructive">{props.testError}</span>
          )}
        </div>
        </>
        )}
      </CardContent>
    </Card>
  );
}

function AgentStep(props: {
  agentName: string;
  setAgentName: (v: string) => void;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Bot className="size-5 text-primary" />
          第一个智能体
        </CardTitle>
        <CardDescription>
          现在只需填写名称，启动后可继续编辑人格、技能和工具。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <div className="space-y-1.5">
          <Label htmlFor="ob-agent">智能体名称</Label>
          <Input
            id="ob-agent"
            value={props.agentName}
            onChange={(e) => props.setAgentName(e.target.value)}
            placeholder="默认"
          />
          <p className="text-xs text-muted-foreground">
            智能体会获得全局唯一 ID（例如{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">agt_a1b2c3…</code>);
            此名称仅用于显示。
          </p>
        </div>
      </CardContent>
    </Card>
  );
}

function SandboxStep(props: {
  enabled: boolean;
  setEnabled: (v: boolean) => void;
  backend: string;
  setBackend: (v: string) => void;
  dockerImage: string;
  setDockerImage: (v: string) => void;
  e2bTemplate: string;
  setE2BTemplate: (v: string) => void;
  e2bKey: string;
  setE2BKey: (v: string) => void;
  boxliteImage: string;
  setBoxliteImage: (v: string) => void;
  boxliteKey: string;
  setBoxliteKey: (v: string) => void;
  boxliteURL: string;
  setBoxliteURL: (v: string) => void;
}) {
  const SANDBOX_BACKEND_LABELS: Record<string, string> = {
    docker: "Docker",
    e2b: "E2B（云端）",
    boxlite: "BoxLite（云端）",
  };
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Container className="size-5 text-primary" />
          沙箱（可选）
        </CardTitle>
        <CardDescription>
          在隔离环境中运行智能体执行的代码。如果不确定，可以先跳过，之后可在“设置”中启用。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-center justify-between">
          <div>
            <p className="text-sm font-medium">启用沙箱</p>
            <p className="text-xs text-muted-foreground">
              默认关闭。代码会在智能体自己的工作区中运行。
            </p>
          </div>
          <Switch checked={props.enabled} onCheckedChange={props.setEnabled} />
        </div>
        {props.enabled && (
          <>
            <Separator />
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>后端</Label>
                <Select
                  value={props.backend}
                  onValueChange={(v) => v && props.setBackend(v)}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue>
                      {(v: unknown) =>
                        SANDBOX_BACKEND_LABELS[v as string] ?? (v as string) ?? ""
                      }
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="docker">Docker</SelectItem>
                    <SelectItem value="e2b">E2B（云端）</SelectItem>
                    <SelectItem value="boxlite">BoxLite（云端）</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              {props.backend === "e2b" ? (
                <>
                  <div className="space-y-1.5">
                    <Label>E2B API 密钥</Label>
                    <Input
                      type="password"
                      value={props.e2bKey}
                      onChange={(e) => props.setE2BKey(e.target.value)}
                      placeholder="e2b_…"
                      className="font-mono text-sm"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label>E2B 模板</Label>
                    <Input
                      value={props.e2bTemplate}
                      onChange={(e) => props.setE2BTemplate(e.target.value)}
                      placeholder="base"
                      className="font-mono text-sm"
                    />
                  </div>
                </>
              ) : props.backend === "boxlite" ? (
                <>
                  <div className="space-y-1.5">
                    <Label>BoxLite API 密钥</Label>
                    <Input
                      type="password"
                      value={props.boxliteKey}
                      onChange={(e) => props.setBoxliteKey(e.target.value)}
                      placeholder="client_secret"
                      className="font-mono text-sm"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label>快照</Label>
                    <Input
                      value={props.boxliteImage}
                      onChange={(e) => props.setBoxliteImage(e.target.value)}
                      placeholder="bkclaw-sandbox"
                      className="font-mono text-sm"
                    />
                    <p className="text-xs text-muted-foreground">
                      BoxLite 快照名称（通过 BoxLite 仪表盘导入），不是 Docker Hub 镜像地址。
                    </p>
                  </div>
                  <div className="space-y-1.5 sm:col-span-2">
                    <Label>API 地址（可选）</Label>
                    <Input
                      value={props.boxliteURL}
                      onChange={(e) => props.setBoxliteURL(e.target.value)}
                      placeholder="https://api.dev.boxlite.ai/api/v1"
                      className="font-mono text-sm"
                    />
                  </div>
                </>
              ) : (
                <div className="space-y-1.5">
                  <Label>Docker 镜像</Label>
                  <Input
                    value={props.dockerImage}
                    onChange={(e) => props.setDockerImage(e.target.value)}
                    placeholder="thinkany/bkclaw-sandbox:latest"
                    className="font-mono text-sm"
                  />
                </div>
              )}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}

function DoneStep({ onContinue }: { onContinue: () => void }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <PartyPopper className="size-5 text-emerald-500" />
          设置完成！
        </CardTitle>
        <CardDescription>
          管理员账户已创建，服务商已配置，第一个智能体已就绪。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <p className="text-sm text-muted-foreground">
          会话 Cookie 已设置，点击继续即可进入仪表盘。
        </p>
      </CardContent>
      <CardFooter>
        <Button onClick={onContinue} className="w-full">
          打开仪表盘 <ArrowRight className="ml-1 size-4" />
        </Button>
      </CardFooter>
    </Card>
  );
}
