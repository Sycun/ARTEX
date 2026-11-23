"use client";

import { useEffect, useState } from "react";

import Link from "next/link";

import { Page } from "@/components/enterprise/shell";
import { Button } from "@/components/ui/button";
import { ent, leaveEnterprise } from "@/lib/enterprise-api";

type Counts = {
  tasks: number;
  findings: number;
  cases: number;
  issued: number;
  cold: number;
  unclaimed: number;
  running: number;
  queued: number;
};

type Sample = {
  findings: number;
  cases: number;
  issued: number;
  cold: number;
  unclaimed: number;
  running: number;
  queued: number;
  message: string;
  loaded?: boolean;
  reused?: boolean;
  current_project?: string;
  baseline?: Counts;
  current?: Counts;
};

export default function AdminPage() {
  const [sample, setSample] = useState<Sample | null>(null);
  const [idpNote, setIdpNote] = useState("");
  const [error, setError] = useState("");
  useEffect(() => {
    void ent<Sample>("/sample")
      .then(setSample)
      .catch((reason: Error) => setError(reason.message));
    void fetch("/api/enterprise/status")
      .then((response) => (response.ok ? response.json() : null))
      .then((status: { idp_note?: string } | null) => setIdpNote(status?.idp_note ?? ""))
      .catch(() => setIdpNote(""));
  }, []);

  async function load() {
    setError("");
    try {
      setSample(await ent<Sample>("/sample/import", { method: "POST" }));
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "导入没有完成");
    }
  }

  async function refresh() {
    setError("");
    try {
      setSample(await ent<Sample>("/sample"));
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "没有读到样例状态");
    }
  }

  const baseline = sample?.baseline;
  const current = sample?.current;

  return (
    <Page title="导入演示样例">
      <div className="max-w-2xl space-y-4 text-sm">
        <p className="text-muted-foreground">
          平台管理用来准备演示：创建项目、任命负责人，并装载
          DEMO-NS-001。成员岗位在项目的成员与职责里授予，由服务端保存。这里不能打开项目里的任务。
        </p>
        {idpNote && <p className="rounded-lg border bg-muted/40 px-3 py-2">{idpNote}</p>}
        <p className="text-muted-foreground">
          DEMO-NS-001 首次装载时热库已签发是 0。历史审核样例 DEMO-RV-001
          是另一份材料，只出现在当前项目计数里，不会改写这份基线，也不会互相覆盖。
        </p>
        <Button onClick={() => void load()}>导入 DEMO-NS-001</Button>
        {error && <p className="text-destructive">{error}</p>}
        {sample && (
          <div className="space-y-3 rounded-xl border p-4">
            <p>{sample.message}</p>
            <div>
              <h2 className="font-medium">首次装载基线</h2>
              <p>项目 A 的脱敏发现：{baseline?.findings ?? sample.findings}</p>
              <p>归并组：{baseline?.cases ?? sample.cases}</p>
              <p>热库中已签发报告：{baseline?.issued ?? sample.issued}</p>
              <p>冷档案：{baseline?.cold ?? sample.cold}，状态可恢复，尚未恢复</p>
              <p>待认领逻辑对象：{baseline?.unclaimed ?? sample.unclaimed}，项目成员不可见</p>
            </div>
            <div>
              <h2 className="font-medium">
                当前项目实际计数{sample.current_project ? ` · ${sample.current_project}` : ""}
              </h2>
              <p>任务：{current?.tasks ?? 0}</p>
              <p>发现：{current?.findings ?? 0}</p>
              <p>归并组：{current?.cases ?? 0}</p>
              <p>已签发报告：{current?.issued ?? 0}</p>
              <p>
                任务启动数 {current?.running ?? sample.running}，排队数 {current?.queued ?? sample.queued}
              </p>
            </div>
            <div className="space-y-2 border-t pt-3">
              <h2 className="font-medium">下一步</h2>
              <p>返回企业首页，可以再看平台管理和项目工作区的区别。</p>
              <p>样例状态就在这张卡片上。再次导入不会清掉报告或审批，也不会多造一份 DEMO-NS-001。</p>
              <p>要进入项目看任务，请退出后使用梁阅登录。这是正常登录，不会把项目权限留在平台管理员身上。</p>
              <div className="flex flex-wrap gap-2">
                <Button asChild variant="outline">
                  <Link href="/enterprise">返回企业首页</Link>
                </Button>
                <Button variant="outline" onClick={() => void refresh()}>
                  查看样例状态
                </Button>
                <Button onClick={() => void leaveEnterprise("/login?switch=reader")}>退出并使用项目审核账号登录</Button>
              </div>
            </div>
          </div>
        )}
      </div>
    </Page>
  );
}
