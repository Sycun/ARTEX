"use client";

import * as React from "react";

import { Loader2Icon, PlusIcon, SendIcon, XIcon } from "lucide-react";

import { HttpCodeBlock } from "@/components/http-code-block";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { type UITranslate, useUITranslation } from "@/i18n/use-ui-translation";
import { ApiError, api } from "@/lib/api";
import { parseRawRequest, splitReplayHeaders } from "@/lib/replay";
import type { TrafficExchange, TrafficReplayHeader, TrafficReplayResult } from "@/lib/types";
import { cn } from "@/lib/utils";

type HeaderRow = TrafficReplayHeader & { key: number };

type ReplayForm = {
  method: string;
  url: string;
  rows: HeaderRow[];
  body: string;
  applyRules: boolean;
  dropped: string[];
  binary: boolean;
  bodyPreviewOnly: boolean;
};

let rowKey = 0;
const toRows = (headers: TrafficReplayHeader[]): HeaderRow[] => headers.map((h) => ({ ...h, key: ++rowKey }));

function statusTone(status: number) {
  if (status >= 500) return "text-red-500";
  if (status >= 400) return "text-amber-500";
  if (status >= 300) return "text-blue-500";
  if (status >= 200) return "text-emerald-500";
  return "text-muted-foreground";
}

function initialForm(exchange: TrafficExchange, raw: string): ReplayForm {
  const parsed = parseRawRequest(raw);
  const { sent, notSent } = splitReplayHeaders(parsed.headers);
  return {
    method: exchange.method.toUpperCase(),
    url: exchange.url,
    rows: toRows(sent),
    body: parsed.binary ? "" : parsed.body,
    applyRules: false,
    dropped: notSent.filter((n) => n.toLowerCase() !== "host"),
    binary: parsed.binary,
    bodyPreviewOnly: parsed.bodyPreviewOnly,
  };
}

function describeError(e: unknown, ui: UITranslate): { title: string; message: string } {
  const message = e instanceof Error ? e.message : String(e);
  if (!(e instanceof ApiError) || !e.status) return { title: ui("traffic.replayFailed"), message };
  if (e.status === 400) return { title: ui("traffic.replayErr400"), message };
  if (e.status === 413) return { title: ui("traffic.replayErr413"), message };
  if (e.status === 502) return { title: ui("traffic.replayErr502"), message };
  if (e.status === 503) return { title: ui("traffic.replayErr503"), message };
  if (e.status === 504) return { title: ui("traffic.replayErr504"), message };
  return { title: ui("traffic.replayFailedStatus", { status: e.status }), message };
}

export function ReplayPanel({
  exchange,
  raw,
  methods,
  onRecorded,
}: {
  exchange: TrafficExchange;
  raw: string;
  methods: string[];
  onRecorded: () => void;
}) {
  const { ui } = useUITranslation();
  const [form, setForm] = React.useState<ReplayForm>(() => initialForm(exchange, raw));
  const [sending, setSending] = React.useState(false);
  const [result, setResult] = React.useState<TrafficReplayResult | null>(null);
  const [error, setError] = React.useState<{ title: string; message: string } | null>(null);
  const generation = React.useRef(0);
  const methodListId = React.useId();
  const applyRulesId = React.useId();

  React.useEffect(() => {
    generation.current++;
    setForm(initialForm(exchange, raw));
    setSending(false);
    setResult(null);
    setError(null);
  }, [exchange, raw]);

  const update = (patch: Partial<ReplayForm>) => setForm((f) => ({ ...f, ...patch }));
  const updateRow = (key: number, patch: Partial<TrafficReplayHeader>) =>
    setForm((f) => ({ ...f, rows: f.rows.map((r) => (r.key === key ? { ...r, ...patch } : r)) }));

  const ignored = new Set(splitReplayHeaders(form.rows).notSent.map((n) => n.toLowerCase()));

  const send = () => {
    const gen = ++generation.current;
    setSending(true);
    setError(null);
    setResult(null);
    api
      .trafficReplay({
        method: form.method.trim(),
        url: form.url.trim(),
        headers: splitReplayHeaders(form.rows).sent,
        body: form.body,
        body_base64: false,
        apply_rules: form.applyRules,
      })
      .then((r) => {
        if (gen !== generation.current) return;
        setResult(r);
        if (r.recorded) onRecorded();
      })
      .catch((e: unknown) => {
        if (gen === generation.current) setError(describeError(e, ui));
      })
      .finally(() => {
        if (gen === generation.current) setSending(false);
      });
  };

  const canSend =
    !sending && !form.binary && !form.bodyPreviewOnly && form.method.trim() !== "" && form.url.trim() !== "";
  const sortedHeaders = (result?.headers ?? [])
    .map((h, position) => ({ ...h, position }))
    .sort((a, b) => a.name.toLowerCase().localeCompare(b.name.toLowerCase()));

  return (
    <div className="min-h-0 flex-1 space-y-4 overflow-auto p-5">
      <div className="flex gap-2">
        <Input
          aria-label={ui("traffic.hookMethodAria")}
          lang="en"
          className="w-32 font-mono uppercase"
          list={methodListId}
          value={form.method}
          onChange={(e) => update({ method: e.target.value.toUpperCase() })}
        />
        <datalist id={methodListId}>
          {methods.map((m) => (
            <option key={m} value={m} />
          ))}
        </datalist>
        <Input
          aria-label={ui("traffic.hookUrlAria")}
          className="flex-1 font-mono"
          value={form.url}
          onChange={(e) => update({ url: e.target.value })}
        />
      </div>

      <div className="space-y-2">
        <div className="flex items-center justify-between">
          <span className="font-medium text-sm">{ui("traffic.replayHeaders")}</span>
          <Button
            size="sm"
            variant="outline"
            onClick={() => update({ rows: [...form.rows, ...toRows([{ name: "", value: "" }])] })}
          >
            <PlusIcon />
            {ui("traffic.replayAddHeader")}
          </Button>
        </div>
        <p className="text-muted-foreground text-xs">{ui("traffic.replayHostNote")}</p>
        {form.dropped.length > 0 && (
          <p className="text-muted-foreground text-xs">
            {ui("traffic.replayDroppedHeaders", { headers: form.dropped.join(", ") })}
          </p>
        )}
        {form.rows.map((row) => (
          <div key={row.key} className="flex items-center gap-2">
            <Input
              aria-label={ui("traffic.replayHeaderName")}
              className={cn("w-48 font-mono text-xs", ignored.has(row.name.trim().toLowerCase()) && "line-through")}
              placeholder={ui("traffic.hookName")}
              value={row.name}
              onChange={(e) => updateRow(row.key, { name: e.target.value })}
            />
            <Input
              aria-label={ui("traffic.replayHeaderValue")}
              className="flex-1 font-mono text-xs"
              placeholder={ui("traffic.replayValuePlaceholder")}
              value={row.value}
              onChange={(e) => updateRow(row.key, { value: e.target.value })}
            />
            <Button
              size="icon"
              variant="ghost"
              aria-label={ui("traffic.replayRemoveHeader")}
              onClick={() => update({ rows: form.rows.filter((r) => r.key !== row.key) })}
            >
              <XIcon />
            </Button>
          </div>
        ))}
        {ignored.size > 0 && <p className="text-muted-foreground text-xs">{ui("traffic.replayStruckNote")}</p>}
      </div>

      <div className="space-y-2">
        <span className="font-medium text-sm">{ui("traffic.replayBody")}</span>
        {form.binary ? (
          <p className="rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-amber-700 text-xs dark:text-amber-400">
            {ui("traffic.replayBinaryBody")}
          </p>
        ) : (
          <>
            <Textarea
              aria-label={ui("traffic.replayBody")}
              className="font-mono text-xs"
              value={form.body}
              onChange={(e) => update({ body: e.target.value })}
            />
            {form.bodyPreviewOnly && (
              <p className="text-amber-700 text-xs dark:text-amber-400">{ui("traffic.replayPreviewOnly")}</p>
            )}
          </>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-4">
        <Button onClick={send} disabled={!canSend}>
          {sending ? <Loader2Icon className="animate-spin" /> : <SendIcon />}
          {ui("traffic.replaySend")}
        </Button>
        <label htmlFor={applyRulesId} className="flex cursor-pointer items-center gap-2 text-xs select-none">
          <Checkbox
            id={applyRulesId}
            checked={form.applyRules}
            onCheckedChange={(checked) => update({ applyRules: checked === true })}
          />
          <span>{ui("traffic.replayRules")}</span>
        </label>
      </div>

      {error && (
        <div className="space-y-1 rounded-md border border-red-500/40 bg-red-500/10 p-3 text-xs">
          <div className="font-medium text-red-600 dark:text-red-400">{error.title}</div>
          <div className="break-all">{error.message}</div>
        </div>
      )}

      {result && (
        <div className="space-y-3">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <Badge variant="secondary" className={cn("font-mono tabular-nums", statusTone(result.status))}>
              {result.status}
            </Badge>
            <span className="text-muted-foreground tabular-nums">{result.duration_ms} ms</span>
            {result.truncated && <Badge variant="outline">{ui("traffic.truncated")}</Badge>}
          </div>
          {!result.recorded && (
            <div className="rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-amber-700 text-xs dark:text-amber-400">
              {ui("traffic.replayPassthrough")}
            </div>
          )}
          {sortedHeaders.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{ui("traffic.replayHeaderCol")}</TableHead>
                  <TableHead>{ui("traffic.replayValuePlaceholder")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {sortedHeaders.map((h) => (
                  <TableRow key={h.position}>
                    <TableCell className="align-top font-mono text-xs">{h.name}</TableCell>
                    <TableCell className="whitespace-normal break-all font-mono text-xs">{h.value}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
          <div className="h-80">
            <HttpCodeBlock raw={result.raw} />
          </div>
        </div>
      )}
    </div>
  );
}
