"use client";

import * as React from "react";

import { PlusIcon, TrashIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { useUITranslation } from "@/i18n/use-ui-translation";
import { api } from "@/lib/api";
import type { TaskCredential } from "@/lib/types";

export function CredentialsTab({ taskId }: { taskId: string }) {
  const { ui } = useUITranslation();
  const [items, setItems] = React.useState<TaskCredential[] | null>(null);
  const [error, setError] = React.useState("");
  const [domain, setDomain] = React.useState("");
  const [username, setUsername] = React.useState("");
  const [password, setPassword] = React.useState("");
  const [note, setNote] = React.useState("");
  const [adding, setAdding] = React.useState(false);

  const load = React.useCallback(() => {
    api
      .taskCredentials(taskId)
      .then((r) => {
        setItems(r);
        setError("");
      })
      .catch((e) => setError((e as Error).message));
  }, [taskId]);

  React.useEffect(() => {
    load();
    const timer = setInterval(() => {
      if (typeof document === "undefined" || !document.hidden) {
        load();
      }
    }, 15000);
    return () => clearInterval(timer);
  }, [load]);

  async function add() {
    if (!domain.trim() || !username.trim()) return;
    setAdding(true);
    try {
      await api.addTaskCredential(taskId, {
        domain: domain.trim(),
        username: username.trim(),
        password,
        note: note.trim(),
      });
      setDomain("");
      setUsername("");
      setPassword("");
      setNote("");
      load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setAdding(false);
    }
  }

  async function remove(id: number) {
    try {
      await api.deleteTaskCredential(taskId, id);
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{ui("taskCredentials.title")}</CardTitle>
        <CardDescription>{ui("taskCredentials.description")}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {error ? <p className="text-destructive text-sm">{error}</p> : null}
        {items === null ? (
          <Skeleton className="h-24 w-full" />
        ) : items.length === 0 ? (
          <Empty>
            <EmptyHeader>
              <EmptyTitle>{ui("taskCredentials.emptyTitle")}</EmptyTitle>
              <EmptyDescription>{ui("taskCredentials.emptyDesc")}</EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <div className="flex flex-col divide-y">
            {items.map((c) => (
              <div key={c.id} className="flex flex-wrap items-center gap-2 py-2.5">
                <code className="font-medium text-sm">{c.domain}</code>
                <span className="text-muted-foreground text-sm">{c.username}</span>
                <Badge variant={c.source === "agent" ? "secondary" : "outline"}>
                  {c.source === "agent" ? ui("taskCredentials.sourceAgent") : ui("taskCredentials.sourceUser")}
                </Badge>
                <div className="ml-auto flex items-center gap-1">
                  <code className="max-w-40 truncate rounded bg-muted px-2 py-0.5 text-sm">
                    {c.password === "" ? ui("taskCredentials.agentGenerates") : ui("taskCredentials.storedHidden")}
                  </code>
                  <Button
                    size="icon"
                    variant="ghost"
                    className="size-7"
                    aria-label={ui("taskCredentials.deleteAria")}
                    onClick={() => void remove(c.id)}
                  >
                    <TrashIcon />
                  </Button>
                </div>
                {c.note ? <p className="w-full text-muted-foreground text-xs">{c.note}</p> : null}
              </div>
            ))}
          </div>
        )}
        <div className="grid gap-2 sm:grid-cols-[1fr_1fr_1fr_auto]">
          <Input
            placeholder={ui("taskCredentials.domainPlaceholder")}
            value={domain}
            onChange={(e) => setDomain(e.target.value)}
          />
          <Input
            placeholder={ui("taskCredentials.usernamePlaceholder")}
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
          <Input
            placeholder={ui("taskCredentials.passwordPlaceholder")}
            type="text"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
          <Button variant="outline" disabled={adding || !domain.trim() || !username.trim()} onClick={() => void add()}>
            <PlusIcon /> {ui("taskCredentials.add")}
          </Button>
          <Input
            className="sm:col-span-4"
            placeholder={ui("taskCredentials.notePlaceholder")}
            value={note}
            onChange={(e) => setNote(e.target.value)}
          />
        </div>
      </CardContent>
    </Card>
  );
}
