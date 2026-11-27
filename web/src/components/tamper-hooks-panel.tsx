"use client";

import * as React from "react";

import {
  AlertTriangleIcon,
  Code2Icon,
  HistoryIcon,
  Loader2Icon,
  PencilIcon,
  PlayIcon,
  PlusIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  ShieldAlertIcon,
  ShieldCheckIcon,
  Trash2Icon,
} from "lucide-react";
import { toast } from "sonner";

import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useUITranslation } from "@/i18n/use-ui-translation";
import { api } from "@/lib/api";
import {
  buildMatchers,
  DEFAULT_REQUEST_HOOK_SOURCE,
  DEFAULT_RESPONSE_HOOK_SOURCE,
  decodeTraceAfter,
  exchangeDetailToDryRunIO,
  formatHeaderLines,
  formatHeaderMatcherLines,
  isTraceLineMasked,
  parseHeaderLines,
} from "@/lib/tamper-hooks";
import type {
  TamperDryRunRequest,
  TamperDryRunResult,
  TamperHook,
  TamperHookPhase,
  TamperHookVersion,
  TrafficExchange,
} from "@/lib/types";

function fmtShortDateTime(ts: string, locale: string) {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString(locale, {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

interface EditorState {
  mode: "create" | "edit";
  id?: number;
  enabled: boolean;
  name: string;
  phase: TamperHookPhase;
  priority: string;
  note: string;
  methods: string;
  host: string;
  urlRegex: string;
  headers: string;
  bodyRegex: string;
  status: string;
  respHeaders: string;
  respBodyRegex: string;
  source: string;
}

function emptyEditor(phase: TamperHookPhase = "request"): EditorState {
  return {
    mode: "create",
    enabled: false,
    name: "",
    phase,
    priority: "100",
    note: "",
    methods: "",
    host: "",
    urlRegex: "",
    headers: "",
    bodyRegex: "",
    status: "",
    respHeaders: "",
    respBodyRegex: "",
    source: phase === "response" ? DEFAULT_RESPONSE_HOOK_SOURCE : DEFAULT_REQUEST_HOOK_SOURCE,
  };
}

function editorFromHook(h: TamperHook): EditorState {
  return {
    mode: "edit",
    id: h.id,
    enabled: h.enabled,
    name: h.name,
    phase: h.phase,
    priority: String(h.priority),
    note: h.note ?? "",
    methods: (h.matchers?.methods ?? []).join(", "),
    host: h.matchers?.host ?? "",
    urlRegex: h.matchers?.url_regex ?? "",
    headers: formatHeaderMatcherLines(h.matchers?.headers),
    bodyRegex: h.matchers?.body_regex ?? "",
    status: (h.matchers?.status ?? []).join(", "),
    respHeaders: formatHeaderMatcherLines(h.matchers?.resp_headers),
    respBodyRegex: h.matchers?.resp_body_regex ?? "",
    source: h.source,
  };
}

interface DryRunTarget {
  label: string;
  hookId?: number;
  inline?: {
    name: string;
    phase: TamperHookPhase;
    matchers: ReturnType<typeof buildMatchers>;
    source: string;
  };
}

export function TamperHooksPanel({ recentExchanges = [] }: { recentExchanges?: TrafficExchange[] }) {
  const { ui, i18n } = useUITranslation();
  const locale = i18n.resolvedLanguage === "en" ? "en-US" : "zh-CN";

  const [hooks, setHooks] = React.useState<TamperHook[]>([]);
  const [enabledByEnv, setEnabledByEnv] = React.useState<boolean>(true);
  const [loading, setLoading] = React.useState(true);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [togglingId, setTogglingId] = React.useState<number | null>(null);

  const [editor, setEditor] = React.useState<EditorState | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [formError, setFormError] = React.useState<string | null>(null);

  const [deleteTarget, setDeleteTarget] = React.useState<TamperHook | null>(null);
  const [deleting, setDeleting] = React.useState(false);

  const [versionsHook, setVersionsHook] = React.useState<TamperHook | null>(null);
  const [versions, setVersions] = React.useState<TamperHookVersion[]>([]);
  const [versionsLoading, setVersionsLoading] = React.useState(false);
  const [expandedVersion, setExpandedVersion] = React.useState<number | null>(null);
  const [rollbackTarget, setRollbackTarget] = React.useState<TamperHookVersion | null>(null);
  const [rollingBack, setRollingBack] = React.useState(false);

  const [dryRunTarget, setDryRunTarget] = React.useState<DryRunTarget | null>(null);
  const [dryRunInputMode, setDryRunInputMode] = React.useState<"synthetic" | "exchange">("synthetic");
  const [dryRunExchangeId, setDryRunExchangeId] = React.useState("");
  const [dryRunLoadingExchange, setDryRunLoadingExchange] = React.useState(false);
  const [dryRunExchangeWarning, setDryRunExchangeWarning] = React.useState<string | null>(null);
  const [dryRunMethod, setDryRunMethod] = React.useState("GET");
  const [dryRunUrl, setDryRunUrl] = React.useState("https://example.test/api/v1/items");
  const [dryRunReqHeaders, setDryRunReqHeaders] = React.useState("Accept: application/json");
  const [dryRunReqBody, setDryRunReqBody] = React.useState("");
  const [dryRunStatus, setDryRunStatus] = React.useState("200");
  const [dryRunRespHeaders, setDryRunRespHeaders] = React.useState("Content-Type: application/json");
  const [dryRunRespBody, setDryRunRespBody] = React.useState('{"ok":true}');
  const [dryRunRunning, setDryRunRunning] = React.useState(false);
  const [dryRunError, setDryRunError] = React.useState<string | null>(null);
  const [dryRunResult, setDryRunResult] = React.useState<TamperDryRunResult | null>(null);

  const loadHooks = React.useCallback(async () => {
    setLoading(true);
    setLoadError(null);
    try {
      const res = await api.trafficHooks();
      setHooks(res.hooks ?? []);
      setEnabledByEnv(Boolean(res.enabled_by_env));
    } catch (e) {
      setLoadError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void loadHooks();
  }, [loadHooks]);

  const handleToggle = async (hook: TamperHook, next: boolean) => {
    setTogglingId(hook.id);
    try {
      const updated = await api.toggleTrafficHook(hook.id, next);
      setHooks((prev) => prev.map((h) => (h.id === hook.id ? updated : h)));
      toast.success(
        next ? ui("traffic.hookEnabledToast", { name: hook.name }) : ui("traffic.hookDisabledToast", { name: hook.name }),
      );
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setTogglingId(null);
    }
  };

  const handleSave = async () => {
    if (!editor) return;
    setFormError(null);
    const priority = Number.parseInt(editor.priority, 10);
    if (Number.isNaN(priority)) {
      setFormError(ui("traffic.hookPriorityIntError"));
      return;
    }
    const payload = {
      name: editor.name.trim(),
      enabled: editor.mode === "create" ? false : editor.enabled,
      phase: editor.phase,
      priority,
      matchers: buildMatchers(editor),
      source: editor.source,
      note: editor.note.trim(),
    };
    setSaving(true);
    try {
      if (editor.mode === "create") {
        await api.createTrafficHook(payload);
        toast.success(ui("traffic.hookCreatedToast"));
      } else if (editor.id !== undefined) {
        await api.updateTrafficHook(editor.id, payload);
        toast.success(ui("traffic.hookUpdatedToast"));
      }
      setEditor(null);
      await loadHooks();
    } catch (e) {
      setFormError((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await api.deleteTrafficHook(deleteTarget.id);
      toast.success(ui("traffic.hookDeletedToast"));
      setDeleteTarget(null);
      await loadHooks();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setDeleting(false);
    }
  };

  const openVersions = async (hook: TamperHook) => {
    setVersionsHook(hook);
    setVersions([]);
    setExpandedVersion(null);
    setVersionsLoading(true);
    try {
      const res = await api.trafficHookVersions(hook.id);
      setVersions(res.versions ?? []);
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setVersionsLoading(false);
    }
  };

  const handleRollback = async () => {
    if (!versionsHook || !rollbackTarget) return;
    setRollingBack(true);
    try {
      const updated = await api.rollbackTrafficHook(versionsHook.id, rollbackTarget.version);
      toast.success(
        ui("traffic.hookRollbackToast", { version: rollbackTarget.version, current: updated.version }),
      );
      setRollbackTarget(null);
      setVersionsHook(updated);
      await loadHooks();
      const res = await api.trafficHookVersions(updated.id);
      setVersions(res.versions ?? []);
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setRollingBack(false);
    }
  };

  const openDryRunForHook = (hook: TamperHook) => {
    setDryRunTarget({ label: `${hook.name} (v${hook.version}, ${hook.phase})`, hookId: hook.id });
    setDryRunError(null);
    setDryRunResult(null);
    setDryRunExchangeWarning(null);
  };

  const openDryRunForDraft = () => {
    if (!editor) return;
    setDryRunTarget({
      label: ui("traffic.hookDraftLabel", {
        name: editor.name.trim() || ui("traffic.hookUntitled"),
        phase: editor.phase,
      }),
      inline: {
        name: editor.name.trim() || "dry-run",
        phase: editor.phase,
        matchers: buildMatchers(editor),
        source: editor.source,
      },
    });
    setDryRunError(null);
    setDryRunResult(null);
    setDryRunExchangeWarning(null);
  };

  const loadExchangeIntoDryRun = async (id: string) => {
    const trimmed = id.trim();
    if (!trimmed) return;
    setDryRunLoadingExchange(true);
    setDryRunError(null);
    setDryRunExchangeWarning(null);
    try {
      const detail = await api.trafficExchange(trimmed);
      const matched = recentExchanges.find((ex) => ex.id === trimmed);
      const converted = exchangeDetailToDryRunIO(detail, matched?.url);
      setDryRunMethod(converted.io.request.method);
      setDryRunUrl(converted.io.request.url);
      setDryRunReqHeaders(formatHeaderLines(converted.io.request.headers));
      setDryRunReqBody(converted.io.request.body);
      setDryRunStatus(String(converted.io.response.status));
      setDryRunRespHeaders(formatHeaderLines(converted.io.response.headers));
      setDryRunRespBody(converted.io.response.body);
      const warnings: string[] = [];
      if (converted.reqBinary || converted.respBinary) {
        warnings.push(ui("traffic.hookWarnBinary"));
      }
      if (converted.reqPreviewOnly || converted.respPreviewOnly) {
        warnings.push(ui("traffic.hookWarnTruncated"));
      }
      if (warnings.length) setDryRunExchangeWarning(warnings.join(" "));
      toast.success(ui("traffic.hookLoadedExchangeToast", { id: trimmed }));
    } catch (e) {
      setDryRunError((e as Error).message);
    } finally {
      setDryRunLoadingExchange(false);
    }
  };

  const runDryRun = async () => {
    if (!dryRunTarget) return;
    setDryRunRunning(true);
    setDryRunError(null);
    setDryRunResult(null);
    const statusNum = Number.parseInt(dryRunStatus, 10);
    const body: TamperDryRunRequest = {
      ...(dryRunTarget.hookId
        ? { hook_id: dryRunTarget.hookId }
        : {
            name: dryRunTarget.inline?.name,
            phase: dryRunTarget.inline?.phase,
            matchers: dryRunTarget.inline?.matchers,
            source: dryRunTarget.inline?.source,
          }),
      request: {
        method: dryRunMethod.trim().toUpperCase() || "GET",
        url: dryRunUrl.trim() || "https://example.test/",
        headers: parseHeaderLines(dryRunReqHeaders),
        body: dryRunReqBody,
      },
      response: {
        status: Number.isNaN(statusNum) ? 200 : statusNum,
        headers: parseHeaderLines(dryRunRespHeaders),
        body: dryRunRespBody,
      },
    };
    try {
      const res = await api.dryRunTrafficHook(body);
      setDryRunResult(res);
    } catch (e) {
      setDryRunError((e as Error).message);
    } finally {
      setDryRunRunning(false);
    }
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {!enabledByEnv && (
        <Alert className="border-amber-500/40 bg-amber-500/10 py-2.5">
          <AlertTriangleIcon className="size-4 text-amber-500" />
          <AlertDescription className="text-xs">{ui("traffic.hooksEnvOff")}</AlertDescription>
        </Alert>
      )}

      {loadError && (
        <Alert variant="destructive" className="py-2">
          <AlertDescription className="text-xs">{loadError}</AlertDescription>
        </Alert>
      )}

      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-xs text-muted-foreground">{ui("traffic.hooksIntro")}</p>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" className="h-8" onClick={() => void loadHooks()} disabled={loading}>
            <RefreshCwIcon className={`size-3.5 ${loading ? "animate-spin" : ""}`} />
            {ui("traffic.hookRefresh")}
          </Button>
          <Button
            size="sm"
            className="h-8"
            onClick={() => {
              setFormError(null);
              setEditor(emptyEditor("request"));
            }}
          >
            <PlusIcon className="size-3.5" />
            {ui("traffic.hookNew")}
          </Button>
        </div>
      </div>

      <Card className="min-h-0 flex-1 overflow-auto p-0">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{ui("traffic.hookName")}</TableHead>
              <TableHead className="w-28">{ui("traffic.hookPhase")}</TableHead>
              <TableHead className="w-24">{ui("traffic.hookPriority")}</TableHead>
              <TableHead className="w-24">{ui("traffic.hookVersion")}</TableHead>
              <TableHead className="w-24">{ui("traffic.hookEnabled")}</TableHead>
              <TableHead className="w-44">{ui("traffic.hookLastError")}</TableHead>
              <TableHead className="w-48 text-right">{ui("traffic.hookActions")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading && hooks.length === 0 ? (
              <TableRow>
                <TableCell colSpan={7} className="py-10 text-center text-sm text-muted-foreground">
                  <Loader2Icon className="mx-auto mb-2 size-5 animate-spin" />
                  {ui("traffic.hooksLoading")}
                </TableCell>
              </TableRow>
            ) : hooks.length === 0 ? (
              <TableRow>
                <TableCell colSpan={7} className="py-10 text-center text-sm text-muted-foreground">
                  {ui("traffic.hooksEmpty")}
                </TableCell>
              </TableRow>
            ) : (
              hooks.map((hook) => (
                <TableRow key={hook.id}>
                  <TableCell>
                    <div className="font-medium">{hook.name}</div>
                    {hook.note && <div className="line-clamp-1 text-xs text-muted-foreground">{hook.note}</div>}
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline" className="font-mono text-xs">
                      {hook.phase}
                    </Badge>
                  </TableCell>
                  <TableCell className="font-mono text-xs tabular-nums">{hook.priority}</TableCell>
                  <TableCell className="font-mono text-xs tabular-nums">v{hook.version}</TableCell>
                  <TableCell>
                    <Switch
                      checked={hook.enabled}
                      disabled={togglingId === hook.id}
                      aria-label={ui("traffic.hookToggleAria", { name: hook.name })}
                      onCheckedChange={(checked) => void handleToggle(hook, checked)}
                    />
                  </TableCell>
                  <TableCell>
                    {hook.load_error ? (
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <Badge variant="destructive" className="gap-1 font-mono text-xs">
                            <ShieldAlertIcon className="size-3" />
                            {ui("traffic.hookLoadError")}
                          </Badge>
                        </TooltipTrigger>
                        <TooltipContent className="max-w-xs font-mono text-xs">{hook.load_error}</TooltipContent>
                      </Tooltip>
                    ) : hook.last_error ? (
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <Badge variant="destructive" className="gap-1 font-mono text-xs">
                            <AlertTriangleIcon className="size-3" />
                            {hook.last_error.class} ({hook.last_error.count})
                          </Badge>
                        </TooltipTrigger>
                        <TooltipContent className="text-xs">
                          {ui("traffic.hookLastSeenAt", { time: fmtShortDateTime(hook.last_error.at, locale) })}
                        </TooltipContent>
                      </Tooltip>
                    ) : (
                      <Badge variant="secondary" className="gap-1 text-xs text-muted-foreground">
                        <ShieldCheckIcon className="size-3" />
                        {ui("traffic.hookNone")}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex items-center justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        title={ui("traffic.hookDryRun")}
                        aria-label={ui("traffic.hookDryRun")}
                        onClick={() => openDryRunForHook(hook)}
                      >
                        <PlayIcon className="size-3.5" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        title={ui("traffic.hookVersions")}
                        aria-label={ui("traffic.hookVersions")}
                        onClick={() => void openVersions(hook)}
                      >
                        <HistoryIcon className="size-3.5" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        title={ui("traffic.hookEdit")}
                        aria-label={ui("traffic.hookEdit")}
                        onClick={() => {
                          setFormError(null);
                          setEditor(editorFromHook(hook));
                        }}
                      >
                        <PencilIcon className="size-3.5" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        title={ui("traffic.hookDelete")}
                        aria-label={ui("traffic.hookDelete")}
                        className="text-destructive hover:text-destructive"
                        onClick={() => setDeleteTarget(hook)}
                      >
                        <Trash2Icon className="size-3.5" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      <Dialog
        open={editor !== null}
        onOpenChange={(open) => {
          if (!open) {
            setEditor(null);
            setFormError(null);
          }
        }}
      >
        <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
          <DialogHeader>
            <DialogTitle>{editor?.mode === "edit" ? ui("traffic.hookEditTitle") : ui("traffic.hookNewTitle")}</DialogTitle>
            <DialogDescription>
              {editor?.mode === "create" ? ui("traffic.hookNewDesc") : ui("traffic.hookEditDesc")}
            </DialogDescription>
          </DialogHeader>

          {editor && (
            <div className="space-y-4 py-2">
              {formError && (
                <Alert variant="destructive">
                  <AlertTriangleIcon className="size-4" />
                  <AlertDescription className="font-mono text-xs whitespace-pre-wrap">{formError}</AlertDescription>
                </Alert>
              )}

              <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                <div className="space-y-1.5 sm:col-span-1">
                  <Label htmlFor="hook-name">{ui("traffic.hookName")}</Label>
                  <Input
                    id="hook-name"
                    value={editor.name}
                    placeholder="sign-api-requests"
                    onChange={(e) => setEditor({ ...editor, name: e.target.value })}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="hook-phase">{ui("traffic.hookPhase")}</Label>
                  <Select
                    value={editor.phase}
                    onValueChange={(val) => {
                      const nextPhase = val as TamperHookPhase;
                      const shouldSwapTemplate =
                        editor.mode === "create" &&
                        (editor.source.trim() === DEFAULT_REQUEST_HOOK_SOURCE.trim() ||
                          editor.source.trim() === DEFAULT_RESPONSE_HOOK_SOURCE.trim());
                      setEditor({
                        ...editor,
                        phase: nextPhase,
                        source: shouldSwapTemplate
                          ? nextPhase === "response"
                            ? DEFAULT_RESPONSE_HOOK_SOURCE
                            : DEFAULT_REQUEST_HOOK_SOURCE
                          : editor.source,
                      });
                    }}
                  >
                    <SelectTrigger id="hook-phase">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="request">request</SelectItem>
                      <SelectItem value="response">response</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="hook-priority">{ui("traffic.hookPriority")}</Label>
                  <Input
                    id="hook-priority"
                    type="number"
                    value={editor.priority}
                    onChange={(e) => setEditor({ ...editor, priority: e.target.value })}
                  />
                </div>
              </div>

              <div className="space-y-1.5">
                <Label htmlFor="hook-note">{ui("traffic.hookNote")}</Label>
                <Input
                  id="hook-note"
                  value={editor.note}
                  placeholder={ui("traffic.hookNotePlaceholder")}
                  onChange={(e) => setEditor({ ...editor, note: e.target.value })}
                />
              </div>

              <div className="space-y-3 rounded-md border p-3">
                <div className="text-xs font-medium text-muted-foreground">{ui("traffic.hookMatchersHeading")}</div>
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <div className="space-y-1">
                    <Label htmlFor="m-methods" className="text-xs">
                      {ui("traffic.hookMethodsLabel")}
                    </Label>
                    <Input
                      id="m-methods"
                      className="h-8 font-mono text-xs"
                      value={editor.methods}
                      placeholder="POST, PUT"
                      onChange={(e) => setEditor({ ...editor, methods: e.target.value })}
                    />
                  </div>
                  <div className="space-y-1">
                    <Label htmlFor="m-host" className="text-xs">
                      {ui("traffic.hookHostLabel")}
                    </Label>
                    <Input
                      id="m-host"
                      className="h-8 font-mono text-xs"
                      value={editor.host}
                      placeholder="*.example.test"
                      onChange={(e) => setEditor({ ...editor, host: e.target.value })}
                    />
                  </div>
                  <div className="space-y-1">
                    <Label htmlFor="m-url" className="text-xs">
                      {ui("traffic.hookUrlRegexLabel")}
                    </Label>
                    <Input
                      id="m-url"
                      className="h-8 font-mono text-xs"
                      value={editor.urlRegex}
                      placeholder="^/api/v1/"
                      onChange={(e) => setEditor({ ...editor, urlRegex: e.target.value })}
                    />
                  </div>
                  <div className="space-y-1">
                    <Label htmlFor="m-body" className="text-xs">
                      {ui("traffic.hookReqBodyRegexLabel")}
                    </Label>
                    <Input
                      id="m-body"
                      className="h-8 font-mono text-xs"
                      value={editor.bodyRegex}
                      placeholder="token"
                      onChange={(e) => setEditor({ ...editor, bodyRegex: e.target.value })}
                    />
                  </div>
                </div>
                <div className="space-y-1">
                  <Label htmlFor="m-headers" className="text-xs">
                    {ui("traffic.hookReqHeadersLabel")}
                  </Label>
                  <Textarea
                    id="m-headers"
                    rows={2}
                    className="font-mono text-xs"
                    value={editor.headers}
                    placeholder={"Content-Type: application/json\nX-Api-Key"}
                    onChange={(e) => setEditor({ ...editor, headers: e.target.value })}
                  />
                </div>

                {editor.phase === "response" && (
                  <div className="grid grid-cols-1 gap-3 border-t pt-3 sm:grid-cols-2">
                    <div className="space-y-1">
                      <Label htmlFor="m-status" className="text-xs">
                        {ui("traffic.hookStatusCodesLabel")}
                      </Label>
                      <Input
                        id="m-status"
                        className="h-8 font-mono text-xs"
                        value={editor.status}
                        placeholder="200, 2xx"
                        onChange={(e) => setEditor({ ...editor, status: e.target.value })}
                      />
                    </div>
                    <div className="space-y-1">
                      <Label htmlFor="m-resp-body" className="text-xs">
                        {ui("traffic.hookRespBodyRegexLabel")}
                      </Label>
                      <Input
                        id="m-resp-body"
                        className="h-8 font-mono text-xs"
                        value={editor.respBodyRegex}
                        onChange={(e) => setEditor({ ...editor, respBodyRegex: e.target.value })}
                      />
                    </div>
                    <div className="space-y-1 sm:col-span-2">
                      <Label htmlFor="m-resp-headers" className="text-xs">
                        {ui("traffic.hookRespHeadersLabel")}
                      </Label>
                      <Textarea
                        id="m-resp-headers"
                        rows={2}
                        className="font-mono text-xs"
                        value={editor.respHeaders}
                        placeholder="Content-Type: application/json"
                        onChange={(e) => setEditor({ ...editor, respHeaders: e.target.value })}
                      />
                    </div>
                  </div>
                )}
              </div>

              <div className="space-y-1.5">
                <div className="flex items-center justify-between">
                  <Label htmlFor="hook-source" className="flex items-center gap-1.5">
                    <Code2Icon className="size-3.5" />
                    {ui("traffic.hookSource")}
                  </Label>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="h-6 px-2 text-xs"
                    onClick={() =>
                      setEditor({
                        ...editor,
                        source:
                          editor.phase === "response" ? DEFAULT_RESPONSE_HOOK_SOURCE : DEFAULT_REQUEST_HOOK_SOURCE,
                      })
                    }
                  >
                    {ui("traffic.hookResetTemplate")}
                  </Button>
                </div>
                <Textarea
                  id="hook-source"
                  rows={12}
                  spellCheck={false}
                  className="font-mono text-xs leading-relaxed"
                  value={editor.source}
                  onChange={(e) => setEditor({ ...editor, source: e.target.value })}
                />
                <p className="text-xs text-muted-foreground">
                  {editor.phase === "response" ? ui("traffic.hookEntrypointResponse") : ui("traffic.hookEntrypointRequest")}
                </p>
              </div>
            </div>
          )}

          <DialogFooter className="gap-2 sm:justify-between">
            <Button type="button" variant="outline" size="sm" onClick={openDryRunForDraft} disabled={!editor}>
              <PlayIcon className="size-3.5" />
              {ui("traffic.hookDryRunDraft")}
            </Button>
            <div className="flex items-center gap-2">
              <Button type="button" variant="outline" onClick={() => setEditor(null)} disabled={saving}>
                {ui("traffic.hookCancel")}
              </Button>
              <Button type="button" onClick={() => void handleSave()} disabled={saving}>
                {saving && <Loader2Icon className="size-3.5 animate-spin" />}
                {editor?.mode === "edit" ? ui("traffic.hookSave") : ui("traffic.hookCreate")}
              </Button>
            </div>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={deleteTarget !== null} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{ui("traffic.hookDeleteTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {ui("traffic.hookDeleteDesc", { name: deleteTarget?.name ?? "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>{ui("traffic.hookCancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                void handleDelete();
              }}
              disabled={deleting}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {deleting && <Loader2Icon className="size-3.5 animate-spin" />}
              {ui("traffic.hookDelete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Sheet open={versionsHook !== null} onOpenChange={(open) => !open && setVersionsHook(null)}>
        <SheetContent className="w-full overflow-y-auto px-5 pb-6 sm:max-w-2xl">
          <SheetHeader>
            <SheetTitle>{ui("traffic.hookVersionsTitle", { name: versionsHook?.name ?? "" })}</SheetTitle>
            <SheetDescription>{ui("traffic.hookVersionsDesc")}</SheetDescription>
          </SheetHeader>

          <div className="mt-4 space-y-3">
            {versionsLoading ? (
              <div className="py-10 text-center text-sm text-muted-foreground">
                <Loader2Icon className="mx-auto mb-2 size-5 animate-spin" />
                {ui("traffic.hookVersionsLoading")}
              </div>
            ) : versions.length === 0 ? (
              <div className="py-8 text-center text-sm text-muted-foreground">{ui("traffic.hookVersionsEmpty")}</div>
            ) : (
              versions.map((v) => {
                const isCurrent = versionsHook?.version === v.version;
                const isExpanded = expandedVersion === v.version;
                return (
                  <Card key={v.id} className="space-y-2 p-3">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div className="flex items-center gap-2">
                        <Badge variant={isCurrent ? "default" : "outline"} className="font-mono text-xs">
                          v{v.version}
                        </Badge>
                        {isCurrent && <Badge variant="secondary">{ui("traffic.hookCurrent")}</Badge>}
                        <Badge variant="outline" className="font-mono text-xs">
                          {v.phase}
                        </Badge>
                        <span className="font-mono text-xs text-muted-foreground">
                          {ui("traffic.hookPrioShort", { n: v.priority })} · {(v.source_sha256 ?? "").slice(0, 12)}
                        </span>
                      </div>
                      <span className="text-xs text-muted-foreground">{fmtShortDateTime(v.created_at, locale)}</span>
                    </div>
                    {v.note && <p className="text-xs text-muted-foreground">{v.note}</p>}
                    <div className="flex items-center justify-between gap-2 pt-1">
                      <Button
                        variant="ghost"
                        size="sm"
                        className="h-7 px-2 text-xs"
                        onClick={() => setExpandedVersion(isExpanded ? null : v.version)}
                      >
                        <Code2Icon className="size-3.5" />
                        {isExpanded ? ui("traffic.hookHideSource") : ui("traffic.hookViewSource")}
                      </Button>
                      {!isCurrent && (
                        <Button
                          variant="outline"
                          size="sm"
                          className="h-7 px-2.5 text-xs"
                          onClick={() => setRollbackTarget(v)}
                        >
                          <RotateCcwIcon className="size-3.5" />
                          {ui("traffic.hookRollbackTo", { version: v.version })}
                        </Button>
                      )}
                    </div>
                    {isExpanded && (
                      <pre className="max-h-72 overflow-auto rounded-md border bg-muted/40 p-2.5 font-mono text-xs">
                        {v.source}
                      </pre>
                    )}
                  </Card>
                );
              })
            )}
          </div>
        </SheetContent>
      </Sheet>

      <AlertDialog open={rollbackTarget !== null} onOpenChange={(open) => !open && setRollbackTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {ui("traffic.hookRollbackTitle", { version: rollbackTarget?.version ?? 0 })}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {ui("traffic.hookRollbackDesc", {
                version: rollbackTarget?.version ?? 0,
                name: versionsHook?.name ?? "",
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={rollingBack}>{ui("traffic.hookCancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                void handleRollback();
              }}
              disabled={rollingBack}
            >
              {rollingBack && <Loader2Icon className="size-3.5 animate-spin" />}
              {ui("traffic.hookRollback")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Sheet open={dryRunTarget !== null} onOpenChange={(open) => !open && setDryRunTarget(null)}>
        <SheetContent className="w-full overflow-y-auto px-5 pb-6 sm:max-w-3xl">
          <SheetHeader>
            <SheetTitle>{ui("traffic.hookDryRunTitle", { target: dryRunTarget?.label ?? "" })}</SheetTitle>
            <SheetDescription>{ui("traffic.hookDryRunDesc")}</SheetDescription>
          </SheetHeader>

          <div className="mt-4 space-y-4">
            <Tabs value={dryRunInputMode} onValueChange={(v) => setDryRunInputMode(v as "synthetic" | "exchange")}>
              <TabsList className="grid w-full grid-cols-2">
                <TabsTrigger value="exchange">{ui("traffic.hookFromExchangeId")}</TabsTrigger>
                <TabsTrigger value="synthetic">{ui("traffic.hookSyntheticInput")}</TabsTrigger>
              </TabsList>

              <TabsContent value="exchange" className="space-y-3 pt-2">
                <div className="flex items-end gap-2">
                  <div className="flex-1 space-y-1">
                    <Label htmlFor="dryrun-ex-id" className="text-xs">
                      {ui("traffic.hookExchangeIdLabel")}
                    </Label>
                    <Input
                      id="dryrun-ex-id"
                      className="h-8 font-mono text-xs"
                      value={dryRunExchangeId}
                      placeholder={ui("traffic.hookExchangeIdPlaceholder")}
                      onChange={(e) => setDryRunExchangeId(e.target.value)}
                    />
                  </div>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="h-8"
                    disabled={dryRunLoadingExchange || !dryRunExchangeId.trim()}
                    onClick={() => void loadExchangeIntoDryRun(dryRunExchangeId)}
                  >
                    {dryRunLoadingExchange && <Loader2Icon className="size-3.5 animate-spin" />}
                    {ui("traffic.hookLoadExchange")}
                  </Button>
                </div>

                {recentExchanges.length > 0 && (
                  <div className="space-y-1">
                    <Label className="text-xs text-muted-foreground">{ui("traffic.hookPickRecent")}</Label>
                    <Select
                      value=""
                      onValueChange={(id) => {
                        setDryRunExchangeId(id);
                        void loadExchangeIntoDryRun(id);
                      }}
                    >
                      <SelectTrigger className="h-8 font-mono text-xs">
                        <SelectValue placeholder={ui("traffic.hookPickRecentPlaceholder")} />
                      </SelectTrigger>
                      <SelectContent>
                        {recentExchanges.slice(0, 20).map((ex) => (
                          <SelectItem key={ex.id} value={ex.id} className="font-mono text-xs">
                            {ex.method} {ex.host}
                            {ex.url.replace(/^https?:\/\/[^/]+/, "").slice(0, 48)} ({ex.status})
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                )}

                {dryRunExchangeWarning && (
                  <Alert className="border-amber-500/40 bg-amber-500/10 py-2">
                    <AlertTriangleIcon className="size-4 text-amber-500" />
                    <AlertDescription className="text-xs">{dryRunExchangeWarning}</AlertDescription>
                  </Alert>
                )}
              </TabsContent>

              <TabsContent value="synthetic" className="pt-1">
                <p className="text-xs text-muted-foreground">{ui("traffic.hookSyntheticHint")}</p>
              </TabsContent>
            </Tabs>

            <div className="grid grid-cols-1 gap-3 rounded-md border p-3 sm:grid-cols-2">
              <div className="space-y-2">
                <div className="text-xs font-medium">{ui("traffic.hookRequestLabel")}</div>
                <div className="flex gap-2">
                  <Input
                    className="h-8 w-24 font-mono text-xs"
                    value={dryRunMethod}
                    onChange={(e) => setDryRunMethod(e.target.value)}
                    aria-label={ui("traffic.hookMethodAria")}
                  />
                  <Input
                    className="h-8 flex-1 font-mono text-xs"
                    value={dryRunUrl}
                    onChange={(e) => setDryRunUrl(e.target.value)}
                    aria-label={ui("traffic.hookUrlAria")}
                  />
                </div>
                <Textarea
                  rows={3}
                  className="font-mono text-xs"
                  value={dryRunReqHeaders}
                  placeholder="Header: value"
                  onChange={(e) => setDryRunReqHeaders(e.target.value)}
                />
                <Textarea
                  rows={3}
                  className="font-mono text-xs"
                  value={dryRunReqBody}
                  placeholder={ui("traffic.hookReqBodyPlaceholder")}
                  onChange={(e) => setDryRunReqBody(e.target.value)}
                />
              </div>

              <div className="space-y-2">
                <div className="text-xs font-medium">{ui("traffic.hookResponseLabel")}</div>
                <Input
                  className="h-8 w-28 font-mono text-xs"
                  type="number"
                  value={dryRunStatus}
                  onChange={(e) => setDryRunStatus(e.target.value)}
                  aria-label={ui("traffic.hookStatusAria")}
                />
                <Textarea
                  rows={3}
                  className="font-mono text-xs"
                  value={dryRunRespHeaders}
                  placeholder="Header: value"
                  onChange={(e) => setDryRunRespHeaders(e.target.value)}
                />
                <Textarea
                  rows={3}
                  className="font-mono text-xs"
                  value={dryRunRespBody}
                  placeholder={ui("traffic.hookRespBodyPlaceholder")}
                  onChange={(e) => setDryRunRespBody(e.target.value)}
                />
              </div>
            </div>

            <div className="flex justify-end">
              <Button type="button" size="sm" onClick={() => void runDryRun()} disabled={dryRunRunning}>
                {dryRunRunning ? <Loader2Icon className="size-3.5 animate-spin" /> : <PlayIcon className="size-3.5" />}
                {ui("traffic.hookRunDryRun")}
              </Button>
            </div>

            {dryRunError && (
              <Alert variant="destructive">
                <AlertTriangleIcon className="size-4" />
                <AlertDescription className="font-mono text-xs whitespace-pre-wrap">{dryRunError}</AlertDescription>
              </Alert>
            )}

            {dryRunResult && (
              <div className="space-y-3 border-t pt-3">
                <div className="flex items-center justify-between">
                  <span className="text-sm font-medium">{ui("traffic.hookDryRunResult")}</span>
                  <Badge variant={dryRunResult.changed ? "default" : "secondary"}>
                    {dryRunResult.changed ? ui("traffic.hookModified") : ui("traffic.hookUnchanged")}
                  </Badge>
                </div>

                <div className="space-y-1.5">
                  <div className="text-xs font-medium text-muted-foreground">
                    {ui("traffic.hookTraceLines", { count: dryRunResult.lines?.length ?? 0 })}
                  </div>
                  {!dryRunResult.lines || dryRunResult.lines.length === 0 ? (
                    <p className="text-xs text-muted-foreground">{ui("traffic.hookNoTraceLines")}</p>
                  ) : (
                    <div className="overflow-x-auto rounded-md border">
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead className="text-xs">{ui("traffic.hookTraceField")}</TableHead>
                            <TableHead className="text-xs">{ui("traffic.hookTraceOutcome")}</TableHead>
                            <TableHead className="text-xs">{ui("traffic.hookTraceAfter")}</TableHead>
                            <TableHead className="text-xs">{ui("traffic.hookTraceHashes")}</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {dryRunResult.lines.map((line, idx) => {
                            const masked = isTraceLineMasked(line);
                            const decodedAfter = decodeTraceAfter(line.After);
                            return (
                              <TableRow key={`${line.Field}-${line.Outcome}-${idx}`}>
                                <TableCell className="font-mono text-xs">
                                  <div>{line.Field || "-"}</div>
                                  <div className="text-[11px] text-muted-foreground">
                                    {line.Phase} · {line.Action}
                                  </div>
                                </TableCell>
                                <TableCell className="text-xs">
                                  <Badge
                                    variant={
                                      line.Outcome === "error"
                                        ? "destructive"
                                        : line.Outcome === "applied"
                                          ? "default"
                                          : "secondary"
                                    }
                                    className="font-mono text-[11px]"
                                  >
                                    {line.Outcome}
                                  </Badge>
                                  {line.Reason && (
                                    <div className="mt-1 font-mono text-[11px] text-muted-foreground">
                                      {line.Reason}
                                    </div>
                                  )}
                                </TableCell>
                                <TableCell className="max-w-xs font-mono text-xs">
                                  {masked ? (
                                    <Badge
                                      variant="outline"
                                      className="border-amber-500/50 text-amber-600 dark:text-amber-400"
                                    >
                                      {ui("traffic.hookMaskedHmac")}
                                    </Badge>
                                  ) : decodedAfter !== null ? (
                                    <span className="break-all">{decodedAfter.slice(0, 160)}</span>
                                  ) : (
                                    <span className="text-muted-foreground">-</span>
                                  )}
                                </TableCell>
                                <TableCell className="font-mono text-[11px] text-muted-foreground">
                                  {line.BeforeHash && <div>{line.BeforeHash.slice(0, 12)}</div>}
                                  {line.AfterHash && <div>{"-> "}{line.AfterHash.slice(0, 12)}</div>}
                                </TableCell>
                              </TableRow>
                            );
                          })}
                        </TableBody>
                      </Table>
                    </div>
                  )}
                </div>

                <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                  <div className="space-y-1">
                    <div className="text-xs font-medium text-muted-foreground">{ui("traffic.hookResultingReq")}</div>
                    <pre className="max-h-48 overflow-auto rounded-md border bg-muted/40 p-2 font-mono text-xs">
                      {`${dryRunResult.request.method} ${dryRunResult.request.url}\n${formatHeaderLines(dryRunResult.request.headers)}${dryRunResult.request.body ? `\n\n${dryRunResult.request.body}` : ""}`}
                    </pre>
                  </div>
                  <div className="space-y-1">
                    <div className="text-xs font-medium text-muted-foreground">{ui("traffic.hookResultingResp")}</div>
                    <pre className="max-h-48 overflow-auto rounded-md border bg-muted/40 p-2 font-mono text-xs">
                      {`HTTP ${dryRunResult.response.status}\n${formatHeaderLines(dryRunResult.response.headers)}${dryRunResult.response.body ? `\n\n${dryRunResult.response.body}` : ""}`}
                    </pre>
                  </div>
                </div>
              </div>
            )}
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
