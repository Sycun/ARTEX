"use client";

import * as React from "react";

import { Loader2Icon, RefreshCwIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { type UITranslate, useUITranslation } from "@/i18n/use-ui-translation";
import { api } from "@/lib/api";
import { createTrafficStreamPager, emptyTrafficStreamState } from "@/lib/traffic-stream";
import type { TrafficStreamDirection, TrafficStreamMessage } from "@/lib/types";

const HEX_PREVIEW_BYTES = 64 * 1024;

function fmtBytes(n: number) {
  if (n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1);
  const v = n / 1024 ** i;
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`;
}

function fmtDirection(direction: string, ui: UITranslate) {
  if (direction === "c2s") return ui("traffic.streamC2S");
  if (direction === "s2c") return ui("traffic.streamS2C");
  return direction;
}

function fmtStreamType(type: string, ui: UITranslate) {
  if (type === "text") return ui("traffic.streamTypeText");
  if (type === "binary") return ui("traffic.streamTypeBinary");
  if (type === "event") return ui("traffic.streamTypeEvent");
  return type;
}

function hexDump(bytes: Uint8Array) {
  const lines: string[] = [];
  for (let i = 0; i < bytes.length; i += 16) {
    const row = Array.from(bytes.subarray(i, i + 16), (b) => b.toString(16).padStart(2, "0")).join(" ");
    lines.push(`${i.toString(16).padStart(8, "0")}  ${row}`);
  }
  return lines.join("\n");
}

type FullPayload = { loading: boolean; text?: string; error?: string };

const payloadClass =
  "max-h-64 overflow-auto rounded-md bg-muted/50 p-2 font-mono text-xs break-all whitespace-pre-wrap";

function StreamMessageRow({ message, locale }: { message: TrafficStreamMessage; locale: string }) {
  const { ui } = useUITranslation();
  const [full, setFull] = React.useState<FullPayload | null>(null);
  const binary = message.type === "binary";

  const loadFull = () => {
    if (!message.blob) return;
    setFull({ loading: true });
    api
      .trafficBlob(message.blob)
      .then((bytes) => {
        const text = binary
          ? `${hexDump(bytes.subarray(0, HEX_PREVIEW_BYTES))}\n${ui("traffic.streamBytesTotal", { count: bytes.length })}`
          : new TextDecoder().decode(bytes);
        setFull({ loading: false, text });
      })
      .catch((e: Error) => setFull({ loading: false, error: e.message }));
  };

  if (message.type === "meta") {
    return (
      <div className="mx-5 my-3 space-y-1 rounded-md border border-amber-500/40 bg-amber-500/10 p-3">
        <div className="font-medium text-amber-700 text-xs dark:text-amber-400">
          {ui("traffic.streamLimitReached", { seq: message.seq })}
        </div>
        <pre className={payloadClass}>{message.payload}</pre>
      </div>
    );
  }

  return (
    <div className="space-y-1.5 border-b px-5 py-3 last:border-b-0">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <span className="font-mono text-muted-foreground tabular-nums">#{message.seq}</span>
        <Badge variant="outline">{fmtDirection(message.direction, ui)}</Badge>
        <Badge variant="secondary">{fmtStreamType(message.type, ui)}</Badge>
        {binary && message.encoding === "base64" && <Badge variant="outline">base64</Badge>}
        {message.truncated && <Badge variant="destructive">{ui("traffic.truncated")}</Badge>}
        {message.event && <span className="font-mono">event: {message.event}</span>}
        {message.event_id && <span className="font-mono">id: {message.event_id}</span>}
        {message.retry ? <span className="font-mono">retry: {message.retry}</span> : null}
        <span className="ml-auto text-muted-foreground tabular-nums">
          {fmtBytes(message.size)} · {new Date(message.ts * 1000).toLocaleTimeString(locale)}
        </span>
      </div>
      <pre className={payloadClass}>{full?.text ?? message.payload}</pre>
      {message.blob && (
        <div className="flex flex-wrap items-center gap-2 text-muted-foreground text-xs">
          <span className="break-all font-mono">blob {message.blob}</span>
          {!full?.text && (
            <Button variant="outline" size="xs" disabled={full?.loading} onClick={loadFull}>
              {full?.loading && <Loader2Icon className="animate-spin" />}
              {ui("traffic.streamLoadFull")}
            </Button>
          )}
          {full?.error && <span className="text-destructive">{full.error}</span>}
        </div>
      )}
    </div>
  );
}

export type StreamMessagesProps = { exchangeId: string; kind: "websocket" | "sse" };

export function StreamMessagesPanel(props: StreamMessagesProps) {
  return <StreamMessages key={props.exchangeId} {...props} />;
}

function StreamMessages({ exchangeId, kind }: StreamMessagesProps) {
  const { ui, i18n } = useUITranslation();
  const locale = i18n.resolvedLanguage === "en" ? "en-US" : "zh-CN";
  const [direction, setDirection] = React.useState<TrafficStreamDirection>("");
  const [{ items, total, messageTotal, loading, loadingMore, error }, setPage] =
    React.useState(emptyTrafficStreamState);
  const pager = React.useMemo(() => createTrafficStreamPager(api.trafficStream, setPage), []);
  const messageCount = items.filter((message) => message.type !== "meta").length;

  React.useEffect(() => {
    pager.select(exchangeId, direction);
    return pager.cancel;
  }, [exchangeId, direction, pager]);

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-auto">
      <div className="flex flex-wrap items-center gap-2 px-5 py-3 text-muted-foreground text-xs">
        <span>
          {ui("traffic.streamSummary", {
            kind: kind === "websocket" ? "WebSocket" : "Server-Sent Events",
            count: messageCount,
            total: messageTotal,
          })}
        </span>
        <Select
          value={direction || "all"}
          onValueChange={(value) => {
            const next = value === "all" ? "" : (value as TrafficStreamDirection);
            if (next === direction) return;
            pager.cancel();
            setDirection(next);
          }}
        >
          <SelectTrigger size="sm" className="ml-auto w-44" aria-label={ui("traffic.streamDirection")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{ui("traffic.streamBoth")}</SelectItem>
            <SelectItem value="c2s">{ui("traffic.streamC2S")}</SelectItem>
            <SelectItem value="s2c">{ui("traffic.streamS2C")}</SelectItem>
          </SelectContent>
        </Select>
        <Button variant="outline" size="xs" disabled={loading} onClick={pager.refresh}>
          <RefreshCwIcon className={loading ? "animate-spin" : undefined} />
          {ui("traffic.hookRefresh")}
        </Button>
      </div>
      {loading ? (
        <div className="flex items-center gap-2 p-5 text-muted-foreground text-xs">
          <Loader2Icon className="size-3.5 animate-spin" />
          {ui("traffic.streamLoading")}
        </div>
      ) : error ? (
        <div className="p-5 text-destructive text-xs">{ui("traffic.streamLoadFailed", { error })}</div>
      ) : (
        <>
          {messageTotal === 0 && (
            <div className="p-5 text-muted-foreground text-xs">
              {direction ? ui("traffic.streamEmptyDir") : ui("traffic.streamEmpty")}
            </div>
          )}
          {items.length > 0 && (
            <div className="border-t">
              {items.map((m) => (
                <StreamMessageRow key={`${exchangeId}:${m.seq}`} message={m} locale={locale} />
              ))}
            </div>
          )}
          {items.length < total && (
            <div className="flex justify-center p-4">
              <Button variant="outline" size="sm" disabled={loadingMore} onClick={pager.loadMore}>
                {loadingMore && <Loader2Icon className="animate-spin" />}
                {ui("traffic.streamLoadMore")}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
