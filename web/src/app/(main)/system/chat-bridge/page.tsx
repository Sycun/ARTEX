"use client";

import * as React from "react";

import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";

type Bridge = {
  kind: string;
  configured: boolean;
  enabled: boolean;
  config: Record<string, string>;
  allowed_chats: string[];
  status?: { state?: string; last_error?: string };
};

export default function ChatBridgePage() {
  const { t } = useTranslation();
  const [bridge, setBridge] = React.useState<Bridge | null>(null);
  const [enabled, setEnabled] = React.useState(false);
  const [token, setToken] = React.useState("");
  const [baseUrl, setBaseUrl] = React.useState("");
  const [allowed, setAllowed] = React.useState("");
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(async () => {
    const rows = await api.chatBridges();
    const telegram = rows.find((item) => item.kind === "telegram") ?? null;
    setBridge(telegram);
    setEnabled(Boolean(telegram?.enabled));
    setBaseUrl(telegram?.config?.base_url ?? "");
    setAllowed((telegram?.allowed_chats ?? []).join("\n"));
    setToken("");
  }, []);

  React.useEffect(() => {
    void load().catch((error: Error) => toast.error(t("chatBridge.failed", { error: error.message })));
  }, [load, t]);

  const payload = () => {
    const config: Record<string, string> = {};
    if (token.trim()) config.bot_token = token.trim();
    else if (bridge?.config?.bot_token) config.bot_token = bridge.config.bot_token;
    if (baseUrl.trim()) config.base_url = baseUrl.trim();
    return {
      enabled,
      allowed_chats: allowed
        .split(/\n+/)
        .map((line) => line.trim())
        .filter(Boolean),
      config,
    };
  };

  const save = async () => {
    setBusy(true);
    try {
      await api.saveChatBridge("telegram", payload());
      toast.success(t("chatBridge.saved"));
      await load();
    } catch (error) {
      toast.error(t("chatBridge.failed", { error: (error as Error).message }));
    } finally {
      setBusy(false);
    }
  };

  const test = async () => {
    setBusy(true);
    try {
      await api.testChatBridge("telegram");
      toast.success(t("chatBridge.tested"));
      await load();
    } catch (error) {
      toast.error(t("chatBridge.failed", { error: (error as Error).message }));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4 p-4">
      <Card>
        <CardHeader>
          <CardTitle>{t("chatBridge.title")}</CardTitle>
          <CardDescription>{t("chatBridge.help")}</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex items-center justify-between gap-3">
            <Label htmlFor="bridge-enabled">{t("chatBridge.enabled")}</Label>
            <Switch id="bridge-enabled" checked={enabled} onCheckedChange={setEnabled} />
          </div>
          <p className="text-muted-foreground text-xs">
            {t("chatBridge.state")}: {bridge?.status?.state ?? "off"}
            {bridge?.status?.last_error ? ` · ${bridge.status.last_error}` : ""}
          </p>
          <div className="grid gap-2">
            <Label htmlFor="bridge-token">{t("chatBridge.token")}</Label>
            <Input
              id="bridge-token"
              value={token}
              onChange={(event) => setToken(event.target.value)}
              placeholder={bridge?.config?.bot_token || ""}
            />
            <p className="text-muted-foreground text-xs">{t("chatBridge.tokenHint")}</p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="bridge-base">{t("chatBridge.baseUrl")}</Label>
            <Input
              id="bridge-base"
              value={baseUrl}
              onChange={(event) => setBaseUrl(event.target.value)}
              placeholder="https://api.telegram.org"
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="bridge-chats">{t("chatBridge.allowed")}</Label>
            <Textarea id="bridge-chats" value={allowed} onChange={(event) => setAllowed(event.target.value)} rows={4} />
            <p className="text-muted-foreground text-xs">{t("chatBridge.allowedHint")}</p>
          </div>
          <div className="flex gap-2">
            <Button type="button" onClick={() => void save()} disabled={busy}>
              {t("chatBridge.save")}
            </Button>
            <Button type="button" variant="outline" onClick={() => void test()} disabled={busy || !bridge?.configured}>
              {t("chatBridge.test")}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
