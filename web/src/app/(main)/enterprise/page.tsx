"use client";

import { useEffect, useState } from "react";

import Link from "next/link";

import { Gate, Logout, useMe } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { clearProject, ent, leaveEnterprise, projectPath, type Receipt } from "@/lib/enterprise-api";

export default function EnterpriseHome() {
  const { me, error, reload } = useMe();
  const [code, setCode] = useState("NS-A");
  const [purpose, setPurpose] = useState("参加门户整改演示");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!me || me.gate !== "ready" || me.platform || me.projects.length !== 1) return;
    clearProject();
    window.location.replace(projectPath(me.projects[0].id));
  }, [me]);

  if (error) return <p className="p-8 text-sm">{error}</p>;
  if (!me) return <p className="p-8 text-sm text-muted-foreground">正在确认登录状态…</p>;
  if (me.gate !== "ready") return <Gate me={me} />;
  if (!me.platform && me.projects.length === 1) {
    return <p className="p-8 text-sm text-muted-foreground">正在进入{me.projects[0].name}…</p>;
  }

  async function submitJoin() {
    setBusy(true);
    setNotice("");
    try {
      const receipt = await ent<Receipt>("/join", {
        method: "POST",
        body: JSON.stringify({ code, purpose }),
      });
      setNotice(
        receipt.reused
          ? `这张回执还在处理中，编号 ${receipt.receipt_no}。没有新建另一张。`
          : `已提交。回执 ${receipt.receipt_no}，当前是${receipt.status_label}。`,
      );
      if (receipt.credential_notice) setNotice((prev) => `${prev} ${receipt.credential_notice}`);
      await reload();
    } catch (reason) {
      setNotice(reason instanceof Error ? reason.message : "没有提交成功");
    } finally {
      setBusy(false);
    }
  }

  let title = "已登录，还没有项目";
  if (me.platform && me.projects.length === 0) title = "平台管理";
  if (me.projects.length > 0) title = `${me.name}的项目`;

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-6 p-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="font-semibold text-xl">{title}</h1>
          <p className="text-sm text-muted-foreground">企业账号 {me.name}</p>
        </div>
        <Logout />
      </div>
      {me.platform && (
        <section className="space-y-3 rounded-xl border p-4 text-sm">
          <h2 className="font-medium">平台管理</h2>
          <p>
            平台管理负责初始化授权和导入演示样例。项目工作区是成员查看任务、发现和报告的地方。导入不会把项目数据授给平台管理员。
          </p>
          <p className="text-muted-foreground">
            {me.grant_ready ? me.init_grant : "缺初始化授权单时不能导入。授权单要由企业业务负责人批准。"}
          </p>
          <div className="flex flex-wrap gap-2">
            <Button asChild>
              <Link href="/enterprise/admin">打开导入演示样例</Link>
            </Button>
            <Button variant="outline" onClick={() => void leaveEnterprise("/login?switch=reader")}>
              退出并使用项目审核账号登录
            </Button>
          </div>
          <p className="text-muted-foreground">
            项目审核账号是梁阅，只读进入门户整改。多人演示仍从分析员周宁请求加入开始，由负责人沈昭审批。
          </p>
        </section>
      )}
      {me.projects.length === 0 && !me.platform && (
        <section className="space-y-3 rounded-xl border p-4">
          <p className="text-sm text-muted-foreground">输入项目编号并请求加入。这里不列出项目，也不列出成员。</p>
          <div className="space-y-1.5">
            <Label htmlFor="code">项目编号</Label>
            <Input id="code" value={code} onChange={(event) => setCode(event.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="purpose">用途</Label>
            <Input id="purpose" value={purpose} onChange={(event) => setPurpose(event.target.value)} />
          </div>
          <Button disabled={busy || !code.trim()} onClick={() => void submitJoin()}>
            请求加入
          </Button>
        </section>
      )}
      {notice && <p className="text-sm">{notice}</p>}
      <div className="space-y-3">
        {me.receipts.map((receipt) => (
          <article key={receipt.id} className="space-y-2 rounded-xl border p-4">
            <p className="font-medium">
              回执 {receipt.receipt_no} · {receipt.status_label}
            </p>
            <p className="text-sm text-muted-foreground">编号 {receipt.project_code}</p>
            {receipt.purpose && <p className="text-sm">用途：{receipt.purpose}</p>}
            {receipt.credential_notice && <p className="text-sm">{receipt.credential_notice}</p>}
            {receipt.status === "approved" && receipt.project_id && (
              <Button asChild>
                <Link href={projectPath(receipt.project_id)}>
                  进入项目{receipt.project_name ? ` ${receipt.project_name}` : ""}
                </Link>
              </Button>
            )}
            {receipt.status === "pending" && (
              <p className="text-sm">待处理。批准后可以进入项目，现在还看不到项目名单。</p>
            )}
            {receipt.status === "rejected" && (
              <p className="text-sm">已拒绝。原因是{receipt.structured_reason || "未获批准"}。</p>
            )}
          </article>
        ))}
      </div>
      <div className="flex flex-wrap gap-2">
        {me.projects.map((project) => (
          <Button key={project.id} asChild variant="outline">
            <Link href={projectPath(project.id)}>
              进入 {project.name}（{project.code}）
            </Link>
          </Button>
        ))}
      </div>
      {me.projects.length > 0 && <Runtime />}
    </div>
  );
}

function Runtime() {
  const [text, setText] = useState("");
  useEffect(() => {
    void ent<{ running: number; queued: number }>("/runtime").then((item) => {
      setText(`任务启动数 ${item.running}，排队数 ${item.queued}`);
    });
  }, []);
  return text ? <p className="text-sm text-muted-foreground">{text}</p> : null;
}
