"use client";

import * as React from "react";

import { GaugeIcon, RefreshCwIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";

type NoiseName = "off" | "quiet" | "balanced" | "aggressive";

type NoiseStatus = {
  profile: NoiseName;
  hosts: Array<{ host: string; rpm?: number; inflight?: number }>;
};

const profiles: NoiseName[] = ["off", "quiet", "balanced", "aggressive"];

export function NoiseProfileCard() {
  const { t } = useTranslation();
  const [status, setStatus] = React.useState<NoiseStatus | null>(null);
  const [saving, setSaving] = React.useState(false);

  const refresh = React.useCallback(async () => {
    const next = await api.noiseStatus();
    setStatus(next);
  }, []);

  React.useEffect(() => {
    void refresh().catch((error: Error) => toast.error(error.message));
  }, [refresh]);

  const save = (value: NoiseName) => {
    setSaving(true);
    api
      .setSettings({ noise_profile: value })
      .then(() => {
        toast.success(t("settings.noise.saved", { profile: t(`settings.noise.${value}`) }));
        return refresh();
      })
      .catch((error: Error) => toast.error(t("settings.noise.saveFailed", { error: error.message })))
      .finally(() => setSaving(false));
  };

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <GaugeIcon className="size-4" />
          {t("settings.noise.title")}
        </CardTitle>
        <CardDescription>{t("settings.noise.help")}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <div className="flex items-center gap-2">
          <Select
            value={status?.profile ?? "off"}
            onValueChange={(value) => save(value as NoiseName)}
            disabled={saving}
          >
            <SelectTrigger className="w-48">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {profiles.map((name) => (
                <SelectItem key={name} value={name}>
                  {t(`settings.noise.${name}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button type="button" variant="outline" size="sm" onClick={() => void refresh()}>
            <RefreshCwIcon />
            {t("settings.noise.refresh")}
          </Button>
        </div>
        <p className="text-muted-foreground text-xs">{t(`settings.noise.${status?.profile ?? "off"}Detail`)}</p>
        <div>
          <p className="mb-1 font-medium text-xs">{t("settings.noise.hosts")}</p>
          {(status?.hosts.length ?? 0) === 0 ? (
            <p className="text-muted-foreground text-xs">{t("settings.noise.noHosts")}</p>
          ) : (
            <ul className="text-xs">
              {status?.hosts.map((host) => (
                <li key={host.host} className="flex justify-between gap-3">
                  <span className="font-mono">{host.host}</span>
                  <span>
                    {t("settings.noise.sent")} {host.rpm ?? 0} · {t("settings.noise.inflight")} {host.inflight ?? 0}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
