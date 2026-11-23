"use client";

import { useEffect, useState } from "react";

import Link from "next/link";
import { usePathname } from "next/navigation";

import { Button } from "@/components/ui/button";
import { ent, leaveEnterprise, type Me } from "@/lib/enterprise-api";

export function useMe() {
  const [me, setMe] = useState<Me | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    ent<Me>("/me")
      .then(setMe)
      .catch((reason: Error) => setError(reason.message));
  }, []);
  return { me, error, reload: () => ent<Me>("/me").then(setMe) };
}

export function Gate({ me }: { me: Me }) {
  if (me.gate === "pending_bind") {
    return (
      <section className="mx-auto max-w-lg space-y-4 p-8">
        <h1 className="text-xl font-semibold">企业账号已登录，目录身份尚未核验</h1>
        <p className="text-sm text-muted-foreground">
          还不能进入项目，也没有「请求加入」。请平台管理员核对身份源。核对通过前，不能把这个人加入项目。
        </p>
        <Logout />
      </section>
    );
  }
  return (
    <section className="mx-auto max-w-lg space-y-4 p-8">
      <h1 className="text-xl font-semibold">账号已停用</h1>
      <p className="text-sm text-muted-foreground">
        请由企业目录恢复账号。页面上的联系人是平台管理员{me.contact_name}。停用期间不能进入项目，也没有「请求加入」。
      </p>
      <Logout />
    </section>
  );
}

export function Logout() {
  return (
    <Button variant="outline" onClick={() => void leaveEnterprise("/login")}>
      退出
    </Button>
  );
}

export function Page({ title, children }: { title: string; children: React.ReactNode }) {
  const path = usePathname();
  const project = path.match(/^\/enterprise\/p\/([^/]+)/)?.[1];
  const projectHome = project ? `/enterprise/p/${project}` : "";
  const onProjectHome = projectHome !== "" && path === projectHome;
  let backHref = "/enterprise";
  let backLabel = "企业首页";
  if (project && !onProjectHome) {
    backHref = projectHome;
    backLabel = "回到项目";
  }
  return (
    <div className="flex flex-1 flex-col gap-4 p-6">
      <div className="flex items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">{title}</h1>
        {path !== "/enterprise" && (
          <Button asChild variant="ghost" size="sm">
            <Link href={backHref}>{backLabel}</Link>
          </Button>
        )}
      </div>
      {children}
    </div>
  );
}

export function Notice({ text }: { text: string }) {
  if (!text) return null;
  return <p className="rounded-lg border bg-muted/40 px-3 py-2 text-sm">{text}</p>;
}
