"use client";

import * as React from "react";

import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Textarea } from "@/components/ui/textarea";
import { useUITranslation } from "@/i18n/use-ui-translation";
import { api } from "@/lib/api";
import type { TrafficDetail, TrafficStreamDirection } from "@/lib/types";

export function TrafficHarActions({ query, onImported }: { query: string; onImported: () => void }) {
  const { ui } = useUITranslation();
  const fileRef = React.useRef<HTMLInputElement>(null);
  const [busy, setBusy] = React.useState(false);

  return (
    <>
      <input
        ref={fileRef}
        type="file"
        accept="application/json,.har"
        className="hidden"
        onChange={(event) => {
          const file = event.target.files?.[0];
          event.target.value = "";
          if (!file) return;
          setBusy(true);
          file
            .text()
            .then((raw) => api.trafficImportHAR(raw))
            .then((res) => {
              toast.success(ui("traffic.harImported", { count: res.imported }));
              onImported();
            })
            .catch((err: Error) => toast.error(err.message))
            .finally(() => setBusy(false));
        }}
      />
      <Button variant="outline" size="sm" className="h-8" disabled={busy} onClick={() => fileRef.current?.click()}>
        {ui("traffic.harImport")}
      </Button>
      <Button
        variant="outline"
        size="sm"
        className="h-8"
        disabled={busy}
        onClick={() => {
          setBusy(true);
          api
            .trafficExportHAR(query)
            .catch((err: Error) => toast.error(err.message))
            .finally(() => setBusy(false));
        }}
      >
        {ui("traffic.harExport")}
      </Button>
    </>
  );
}

export function TrafficHooksButton() {
  const { ui } = useUITranslation();
  const [open, setOpen] = React.useState(false);
  const [hooks, setHooks] = React.useState<Awaited<ReturnType<typeof api.trafficHooks>> | null>(null);
  const [name, setName] = React.useState("");
  const [phase, setPhase] = React.useState("request");
  const [source, setSource] = React.useState('req.SetHeader("X-Test", "1")');
  const [dry, setDry] = React.useState("");
  const [versions, setVersions] = React.useState<string>("");

  const load = React.useCallback(() => {
    api
      .trafficHooks()
      .then(setHooks)
      .catch((err: Error) => toast.error(err.message));
  }, []);

  React.useEffect(() => {
    if (open) load();
  }, [open, load]);

  return (
    <>
      <Button variant="outline" size="sm" className="h-8" onClick={() => setOpen(true)}>
        {ui("traffic.hooks")}
      </Button>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent className="w-full overflow-auto sm:max-w-xl">
          <SheetHeader>
            <SheetTitle>{ui("traffic.hooks")}</SheetTitle>
          </SheetHeader>
          <div className="space-y-3 px-4 pb-6 text-sm">
            {hooks && !hooks.enabled_by_env ? (
              <p className="text-muted-foreground">{ui("traffic.hooksEnvOff")}</p>
            ) : null}
            <div className="space-y-2 rounded-md border p-3">
              <Input value={name} placeholder={ui("traffic.hookName")} onChange={(e) => setName(e.target.value)} />
              <Select value={phase} onValueChange={setPhase}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="request">request</SelectItem>
                  <SelectItem value="response">response</SelectItem>
                </SelectContent>
              </Select>
              <Textarea
                value={source}
                onChange={(e) => setSource(e.target.value)}
                className="min-h-28 font-mono text-xs"
              />
              <div className="flex gap-2">
                <Button
                  size="sm"
                  disabled={!name.trim() || !source.trim()}
                  onClick={() => {
                    api
                      .createTrafficHook({ name: name.trim(), phase, priority: 0, source, note: "" })
                      .then(() => {
                        setName("");
                        load();
                      })
                      .catch((err: Error) => toast.error(err.message));
                  }}
                >
                  {ui("traffic.hookCreate")}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    api
                      .dryRunTrafficHook({
                        source,
                        phase,
                        request: { method: "GET", url: "https://example.test/", headers: {}, body: "" },
                        response: { status: 200, headers: {}, body: "ok" },
                      })
                      .then((res) => setDry(JSON.stringify(res, null, 2)))
                      .catch((err: Error) => toast.error(err.message));
                  }}
                >
                  {ui("traffic.hookDryRun")}
                </Button>
              </div>
            </div>
            {(hooks?.hooks ?? []).length === 0 ? (
              <p className="text-muted-foreground">{ui("traffic.hooksEmpty")}</p>
            ) : null}
            {(hooks?.hooks ?? []).map((hook) => (
              <div key={hook.id} className="space-y-2 rounded-md border p-3">
                <div className="flex items-center gap-2">
                  <span className="font-medium">{hook.name}</span>
                  <Badge variant="secondary">{hook.phase}</Badge>
                  <span className="text-muted-foreground text-xs">v{hook.version}</span>
                </div>
                {hook.load_error ? <p className="text-destructive text-xs">{hook.load_error}</p> : null}
                <div className="flex flex-wrap gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() =>
                      api
                        .toggleTrafficHook(hook.id, !hook.enabled)
                        .then(load)
                        .catch((err: Error) => toast.error(err.message))
                    }
                  >
                    {hook.enabled ? ui("traffic.hookDisable") : ui("traffic.hookEnable")}
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() =>
                      api
                        .trafficHookVersions(hook.id)
                        .then((res) => setVersions(JSON.stringify(res.versions, null, 2)))
                        .catch((err: Error) => toast.error(err.message))
                    }
                  >
                    {ui("traffic.hookVersions")}
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => {
                      const version = Number(window.prompt(ui("traffic.hookRollback")) || 0);
                      if (!version) return;
                      api
                        .rollbackTrafficHook(hook.id, version)
                        .then(load)
                        .catch((err: Error) => toast.error(err.message));
                    }}
                  >
                    {ui("traffic.hookRollback")}
                  </Button>
                </div>
              </div>
            ))}
            {dry ? (
              <pre className="overflow-auto rounded-md bg-muted p-2 text-xs">
                {ui("traffic.hookDryRunResult")}
                {"\n"}
                {dry}
              </pre>
            ) : null}
            {versions ? <pre className="overflow-auto rounded-md bg-muted p-2 text-xs">{versions}</pre> : null}
          </div>
        </SheetContent>
      </Sheet>
    </>
  );
}

export function TrafficReplayPanel({ detail }: { detail: TrafficDetail | null }) {
  const { ui } = useUITranslation();
  const [applyRules, setApplyRules] = React.useState(false);
  const [result, setResult] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  if (!detail?.req) return null;
  const first = detail.req.split("\n")[0] ?? "";
  const method = first.split(" ")[0] || "GET";
  return (
    <div className="space-y-2 border-t px-5 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium text-xs">{ui("traffic.replay")}</span>
        <label className="flex items-center gap-1 text-muted-foreground text-xs">
          <input type="checkbox" checked={applyRules} onChange={(e) => setApplyRules(e.target.checked)} />
          {ui("traffic.replayRules")}
        </label>
        <Button
          size="sm"
          disabled={busy}
          onClick={() => {
            const url = window.prompt("URL");
            if (!url) return;
            setBusy(true);
            api
              .trafficReplay({ method, url, body: "", apply_rules: applyRules })
              .then((res) => setResult(res.raw || String(res.status)))
              .catch((err: Error) => toast.error(err.message))
              .finally(() => setBusy(false));
          }}
        >
          {ui("traffic.replaySend")}
        </Button>
      </div>
      {result ? <pre className="max-h-40 overflow-auto rounded-md bg-muted p-2 text-xs">{result}</pre> : null}
    </div>
  );
}

export function TrafficStreamPanel({ exchangeId, kind }: { exchangeId: string; kind?: string }) {
  const { ui } = useUITranslation();
  const [direction, setDirection] = React.useState<TrafficStreamDirection>("");
  const [text, setText] = React.useState("");
  React.useEffect(() => {
    if (!kind) {
      setText("");
      return;
    }
    let alive = true;
    api
      .trafficStream(exchangeId, 0, 50, direction)
      .then((page) => {
        if (!alive) return;
        setText(
          page.items.length
            ? page.items.map((m) => `${m.seq} ${m.direction} ${m.type} ${m.payload}`).join("\n")
            : ui("traffic.streamEmpty"),
        );
      })
      .catch((err: Error) => {
        if (alive) setText(err.message);
      });
    return () => {
      alive = false;
    };
  }, [exchangeId, kind, direction, ui]);
  if (!kind) return null;
  return (
    <div className="space-y-2 border-t px-5 py-3">
      <div className="flex items-center gap-2">
        <span className="font-medium text-xs">{ui("traffic.streamDirection")}</span>
        <Select
          value={direction || "both"}
          onValueChange={(value) => setDirection(value === "both" ? "" : (value as TrafficStreamDirection))}
        >
          <SelectTrigger className="h-8 w-48">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="both">{ui("traffic.streamBoth")}</SelectItem>
            <SelectItem value="c2s">{ui("traffic.streamC2S")}</SelectItem>
            <SelectItem value="s2c">{ui("traffic.streamS2C")}</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <pre className="max-h-40 overflow-auto rounded-md bg-muted p-2 text-xs">{text}</pre>
    </div>
  );
}
