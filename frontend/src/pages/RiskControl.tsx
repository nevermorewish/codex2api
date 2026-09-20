import { useTranslation } from "react-i18next";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useId,
  cloneElement,
  isValidElement,
  type ReactNode,
} from "react";
import { NavLink, Navigate, useParams } from "react-router-dom";
import {
  ShieldCheck,
  Save,
  RefreshCw,
  AlertTriangle,
  Search,
  Trash2,
  FlaskConical,
  ExternalLink,
} from "lucide-react";
import { api } from "../api";
import PageHeader from "../components/PageHeader";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { DraftNumberInput } from "@/components/ui/draft-number-input";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { Switch } from "@/components/ui/switch";
import { Card, CardContent } from "@/components/ui/card";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import RiskModelAudit from "./RiskModelAudit";
import {
  parseRiskWords,
  type RiskConfig,
  type RiskConfigView,
  type RiskStatus,
  type RiskLogPage,
} from "../lib/riskControl";
const textarea =
  "w-full rounded-lg border border-border bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring min-h-28";
function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: ReactNode;
}) {
  const id = useId();
  const control = isValidElement<{
    id?: string;
    "aria-describedby"?: string;
  }>(children)
    ? cloneElement(children, {
        id,
        "aria-describedby": hint ? id + "-hint" : undefined,
      })
    : children;
  return (
    <div className="flex min-w-0 flex-col gap-2 text-sm">
      <label htmlFor={id} className="font-medium">
        {label}
      </label>
      {control}
      {hint && (
        <p
          id={id + "-hint"}
          className="text-xs text-muted-foreground leading-relaxed"
        >
          {hint}
        </p>
      )}
    </div>
  );
}
function Section({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children: ReactNode;
}) {
  return (
    <Card>
      <CardContent className="space-y-5 p-5">
        <div>
          <h3 className="font-semibold">{title}</h3>
          {description && (
            <p className="mt-1 text-sm text-muted-foreground">{description}</p>
          )}
        </div>
        {children}
      </CardContent>
    </Card>
  );
}
function Toggle({
  label,
  checked,
  onChange,
}: {
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
}) {
  return (
    <label className="flex items-center gap-3 text-sm">
      <Switch checked={checked} onCheckedChange={onChange} />
      {label}
    </label>
  );
}
export default function RiskControl() {
  const { t } = useTranslation();
  const views = [
    ["policy", t("riskControl.text001")],
    ["model-audit", t("riskControl.modelAudit.title")],
    ["logs", t("riskControl.text003")],
    ["hashes", t("riskControl.text004")],
    ["test", t("riskControl.text005")],
    ["prompt", t("riskControl.text006")],
  ] as const;
  const { view = "policy" } = useParams();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const ask = (description: string) =>
    confirm({ title: t("riskControl.text007"), description, tone: "warning" });
  const [config, setConfig] = useState<RiskConfigView | null>(null);
  const [form, setForm] = useState<RiskConfig | null>(null);
  const [status, setStatus] = useState<RiskStatus | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [words, setWords] = useState("");
  const [logs, setLogs] = useState<RiskLogPage | null>(null);
  const [logsLoading, setLogsLoading] = useState(true);
  const [logsError, setLogsError] = useState("");
  const [action, setAction] = useState(""),
    [query, setQuery] = useState(""),
    [keyFilter, setKeyFilter] = useState(""),
    [page, setPage] = useState(1);
  const [testText, setTestText] = useState(""),
    [testResult, setTestResult] = useState<Record<string, unknown> | null>(
      null,
    );
  const [hash, setHash] = useState("");
  const logVersion = useRef(0);
  const apply = useCallback((v: RiskConfigView) => {
    setConfig(v);
    setForm(v.config);
    setWords(v.config.blocked_keywords.join("\n"));
    setDirty(false);
  }, []);
  const load = useCallback(async () => {
    try {
      const [c, s] = await Promise.all([
        api.getRiskConfig(),
        api.getRiskStatus(),
      ]);
      apply(c);
      setStatus(s);
      setError("");
    } catch (e) {
      setError(getErrorMessage(e, t("riskControl.text008")));
    }
  }, [apply]);
  useEffect(() => {
    void load();
  }, [load]);
  useEffect(() => {
    const timer = setInterval(() => {
      void api
        .getRiskStatus()
        .then(setStatus)
        .catch(() => {});
    }, 10000);
    return () => clearInterval(timer);
  }, []);
  const loadLogs = useCallback(async () => {
    const version = ++logVersion.current;
    setLogsLoading(true);
    setLogsError("");
    try {
      const p = new URLSearchParams({
        page: String(page),
        page_size: "20",
        action,
        q: query,
        api_key_id: keyFilter,
      });
      const v = await api.getRiskLogs(p.toString());
      if (version === logVersion.current) setLogs(v);
    } catch (e) {
      if (version === logVersion.current)
        setLogsError(getErrorMessage(e, t("riskControl.text009")));
    } finally {
      if (version === logVersion.current) setLogsLoading(false);
    }
  }, [page, action, query, keyFilter, t]);
  useEffect(() => {
    if (view === "logs") void loadLogs();
  }, [view, loadLogs]);
  function patch<K extends keyof RiskConfig>(key: K, value: RiskConfig[K]) {
    setForm((f) => (f ? { ...f, [key]: value } : f));
    setDirty(true);
  }
  async function perform(fn: () => Promise<unknown>, message: string) {
    setBusy(true);
    try {
      await fn();
      showToast(message, "success");
      setStatus(await api.getRiskStatus());
      if (view === "logs") await loadLogs();
    } catch (e) {
      showToast(getErrorMessage(e, t("riskControl.text011")), "error");
    } finally {
      setBusy(false);
    }
  }
  async function save() {
    if (!form) return;
    setBusy(true);
    try {
      const next = {
        ...form,
        blocked_keywords: parseRiskWords(words),
      };
      apply(await api.updateRiskConfig(next));
      setStatus(await api.getRiskStatus());
      showToast(t("riskControl.text012"), "success");
    } catch (e) {
      showToast(getErrorMessage(e, t("riskControl.text013")), "error");
    } finally {
      setBusy(false);
    }
  }
  const number = (
    key: keyof RiskConfig,
    label: string,
    min: number,
    max: number,
    hint?: string,
  ) => (
    <Field label={label} hint={hint}>
      <DraftNumberInput
        min={min}
        max={max}
        value={Number(form?.[key] ?? 0)}
        onValueChange={(value) => patch(key, value)}
      />
    </Field>
  );
  if (view === "keys")
    return <Navigate to="/risk-control/model-audit" replace />;
  if (!views.some(([v]) => v === view))
    return <Navigate to="/risk-control/policy" replace />;
  if (error)
    return (
      <div role="alert" className="rounded-xl border p-6 space-y-4">
        <p>{error}</p>
        <Button onClick={() => void load()}>{t("riskControl.text014")}</Button>
      </div>
    );
  if (!form)
    return (
      <div role="status" className="p-8 text-muted-foreground">
        {t("riskControl.text015")}
      </div>
    );
  return (
    <div className="space-y-5 pb-8">
      {confirmDialog}
      <PageHeader
        title={t("riskControl.text016")}
        description={t("riskControl.text017")}
        actions={
          <>
            <Button
              variant="outline"
              disabled={busy}
              onClick={async () => {
                if (!dirty || (await ask(t("riskControl.text018"))))
                  void load();
              }}
            >
              <RefreshCw className="size-4" />
              {t("riskControl.text019")}
            </Button>
            <Button disabled={busy || !dirty} onClick={() => void save()}>
              <Save className="size-4" />
              {busy ? t("riskControl.text020") : t("riskControl.text021")}
            </Button>
          </>
        }
      />
      <div className="grid grid-cols-2 gap-3 xl:grid-cols-4">
        {[
          [t("riskControl.text003"), status?.total],
          [t("riskControl.text022"), status?.hits],
          [t("riskControl.text023"), status?.blocked],
          [t("riskControl.text024"), status?.hashes],
        ].map(([label, value]) => (
          <Card key={label}>
            <CardContent className="p-4">
              <p className="text-xs text-muted-foreground">{label}</p>
              <p className="mt-2 text-2xl font-semibold tabular-nums">
                {value ?? "—"}
              </p>
            </CardContent>
          </Card>
        ))}
      </div>
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-muted/30 p-4">
        <div className="flex items-center gap-2 text-sm">
          <ShieldCheck className="size-5 text-primary" />
          {config?.config.enabled && config.config.mode !== "off"
            ? t("riskControl.text026")
            : t("riskControl.text027")}
          <span className="text-muted-foreground">
            ·{" "}
            {config?.config.mode === "off"
              ? t("riskControl.text039")
              : config?.config.mode === "observe"
                ? t("riskControl.text028")
                : t("riskControl.text029")}
          </span>
          {dirty && (
            <span className="text-amber-600">{t("riskControl.text030")}</span>
          )}
          <span className="text-xs text-muted-foreground">
            {t("riskControl.modelAudit.title")}
          </span>
        </div>
        <span className="text-xs text-muted-foreground">
          {t("riskControl.runtimeCounts", {
            active: status?.active ?? 0,
            queued: status?.queue_length ?? 0,
            dropped: status?.dropped ?? 0,
            errors: status?.errors ?? 0,
          })}
        </span>
      </div>
      <nav
        aria-label={t("riskControl.text035")}
        className="flex gap-1 overflow-x-auto border-b border-border pb-2"
      >
        {views.map(([v, label]) => (
          <NavLink
            key={v}
            to={"/risk-control/" + v}
            className={({ isActive }) =>
              "whitespace-nowrap rounded-lg px-4 py-2 text-sm " +
              (isActive
                ? "bg-primary text-primary-foreground"
                : "text-muted-foreground hover:bg-muted")
            }
          >
            {label}
          </NavLink>
        ))}
      </nav>

      {view === "model-audit" && config && (
        <RiskModelAudit
          config={form}
          view={config}
          onChange={(next) => {
            setForm(next);
            setDirty(true);
          }}
        />
      )}
      {view === "policy" && (
        <>
          <Section
            title={t("riskControl.text001")}
            description={t("riskControl.text036")}
          >
            <Toggle
              label={t("riskControl.text037")}
              checked={form.enabled}
              onChange={(v) => patch("enabled", v)}
            />
            <div className="grid gap-5 md:grid-cols-3">
              <Field label={t("riskControl.text038")}>
                <Select
                  value={form.mode}
                  onValueChange={(value) =>
                    patch("mode", value as RiskConfig["mode"])
                  }
                  options={[
                    { value: "off", label: t("riskControl.text039") },
                    { value: "observe", label: t("riskControl.text028") },
                    { value: "pre_block", label: t("riskControl.text040") },
                  ]}
                />
              </Field>
              <Field label={t("riskControl.text041")}>
                <Select
                  value={form.keyword_blocking_mode}
                  onValueChange={(value) =>
                    patch(
                      "keyword_blocking_mode",
                      value as RiskConfig["keyword_blocking_mode"],
                    )
                  }
                  options={[
                    {
                      value: "keyword_and_api",
                      label: t("riskControl.text042"),
                    },
                    { value: "keyword_only", label: t("riskControl.text043") },
                    { value: "api_only", label: t("riskControl.text044") },
                  ]}
                />
              </Field>
              {number(
                "sample_rate",
                t("riskControl.text045"),
                0,
                100,
                t("riskControl.text046"),
              )}
            </div>
            <p className="rounded-lg bg-amber-500/10 p-3 text-xs leading-6 text-amber-700 dark:text-amber-300">
              <AlertTriangle className="mr-1 inline size-4" />
              {t("riskControl.modelAudit.runtimeHint")}
            </p>
          </Section>
          <Section
            title={t("riskControl.text048")}
            description={t("riskControl.text049")}
          >
            <textarea
              aria-label={t("riskControl.text048")}
              className={textarea + " min-h-48 font-mono"}
              value={words}
              disabled={form.keyword_blocking_mode === "api_only"}
              onChange={(e) => {
                setWords(e.target.value);
                setDirty(true);
              }}
              placeholder={t("riskControl.text050")}
            />
            <p className="text-xs text-muted-foreground">
              {t("riskControl.keywordCount", {
                count: parseRiskWords(words).length,
              })}{" "}
              {t("riskControl.modelAudit.inputBoundary")}
            </p>
          </Section>
          <Section title={t("riskControl.text066")}>
            <Toggle
              label={t("riskControl.fallbackOnBlock")}
              checked={form.fallback_on_block_enabled ?? true}
              onChange={(v) => patch("fallback_on_block_enabled", v)}
            />
            <p className="text-xs text-muted-foreground">
              {t("riskControl.fallbackOnBlockHint")}
            </p>
            <div className="flex flex-wrap gap-6">
              <Toggle
                label={t("riskControl.text067")}
                checked={form.pre_hash_check_enabled}
                onChange={(v) => patch("pre_hash_check_enabled", v)}
              />
            </div>
            <div className="grid gap-4 md:grid-cols-3">
              {number("block_status", t("riskControl.text069"), 400, 499)}
            </div>
            <Field label={t("riskControl.text072")}>
              <Input
                value={form.block_message}
                onChange={(e) => patch("block_message", e.target.value)}
              />
            </Field>
            <p className="text-xs text-muted-foreground">
              {t("riskControl.text073")}
            </p>
          </Section>
          <Section title={t("riskControl.text074")}>
            <Toggle
              label={t("riskControl.text075")}
              checked={form.record_non_hits}
              onChange={(v) => patch("record_non_hits", v)}
            />
            <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
              {number("worker_count", t("riskControl.text076"), 1, 32)}
              {number("queue_size", t("riskControl.text077"), 1, 100000)}
              {number("hit_retention_days", t("riskControl.text078"), 1, 3650)}
              {number("non_hit_retention_days", t("riskControl.text079"), 1, 3)}
            </div>
            <p className="text-xs text-muted-foreground">
              {t("riskControl.text080")}
            </p>
            <Button
              variant="outline"
              disabled={busy}
              onClick={() =>
                void perform(async () => {
                  const r = await api.cleanupRiskLogs();
                  showToast(
                    t("riskControl.text081", { value0: r.deleted }),
                    "success",
                  );
                }, t("riskControl.text082"))
              }
            >
              {t("riskControl.text083")}
            </Button>
          </Section>
        </>
      )}

      {view === "logs" && (
        <Section
          title={t("riskControl.text003")}
          description={t("riskControl.text124")}
        >
          <div className="flex flex-wrap gap-3">
            <Input
              aria-label={t("riskControl.text125")}
              className="max-w-xs"
              placeholder={t("riskControl.text126")}
              value={query}
              onChange={(e) => {
                setQuery(e.target.value);
                setPage(1);
              }}
            />
            <Input
              aria-label={t("riskControl.text127")}
              className="max-w-40"
              placeholder={t("riskControl.apiKeyID")}
              value={keyFilter}
              onChange={(e) => {
                setKeyFilter(e.target.value);
                setPage(1);
              }}
            />
            <Select
              aria-label={t("riskControl.text128")}
              className="min-w-40"
              value={action}
              onValueChange={(value) => {
                setAction(value);
                setPage(1);
              }}
              options={[
                ["", t("riskControl.text129")],
                ["keyword_block", t("riskControl.text130")],
                ["block", t("riskControl.text131")],
                ["hash_block", t("riskControl.text132")],
                ["flag", t("riskControl.text133")],
                ["allow", t("riskControl.text134")],
                ["error", t("riskControl.text135")],
              ].map(([value, label]) => ({ value, label }))}
            />
            <Button variant="outline" onClick={() => void loadLogs()}>
              <Search className="size-4" />
              {t("riskControl.text136")}
            </Button>
          </div>
          {logsError ? (
            <div role="alert" className="space-y-3 text-destructive">
              <p>{logsError}</p>
              <Button variant="outline" onClick={() => void loadLogs()}>
                {t("riskControl.text014")}
              </Button>
            </div>
          ) : logsLoading || !logs ? (
            <p role="status">{t("riskControl.text137")}</p>
          ) : logs.items.length === 0 ? (
            <div className="rounded-lg border border-dashed p-10 text-center text-muted-foreground">
              {t("riskControl.text138")}
            </div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full min-w-[850px] text-left text-sm">
                <thead>
                  <tr>
                    {[
                      t("riskControl.text139"),
                      t("riskControl.text140"),
                      t("riskControl.text141"),
                      t("riskControl.text142"),
                      t("riskControl.text114"),
                    ].map((x) => (
                      <th key={x} className="p-3">
                        {x}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {logs.items.map((e) => (
                    <tr className="border-t align-top" key={e.id}>
                      <td className="p-3 whitespace-nowrap">
                        {new Date(e.created_at).toLocaleString()}
                        <p className="text-muted-foreground">
                          #{e.api_key_id} {e.api_key_name}
                        </p>
                      </td>
                      <td className="p-3">
                        {e.model}
                        <p className="text-xs text-muted-foreground">
                          {e.endpoint}
                        </p>
                      </td>
                      <td className="p-3">
                        <span
                          className={
                            e.flagged ? "text-red-600" : "text-muted-foreground"
                          }
                        >
                          {e.action}
                        </span>
                        {e.error && <p>{e.error}</p>}
                      </td>
                      <td className="max-w-sm p-3 break-words">
                        <strong>{e.matched_keyword}</strong>
                        <p className="text-xs leading-5 text-muted-foreground">
                          {e.input_excerpt}
                        </p>
                        <details className="mt-2 text-xs">
                          <summary className="cursor-pointer">
                            {t("riskControl.text144")}
                          </summary>
                          <pre className="max-w-xs overflow-auto py-2">
                            {JSON.stringify(e.audit ?? e.scores ?? {}, null, 2)}
                          </pre>
                          <p className="break-all font-mono">{e.input_hash}</p>
                        </details>
                      </td>
                      <td className="p-3 whitespace-nowrap">
                        {e.latency_ms} ms
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div className="flex items-center justify-between">
            <span className="text-xs text-muted-foreground">
              {t("riskControl.pagination", { total: logs?.total ?? 0, page })}
            </span>
            <div className="flex gap-2">
              <Button
                variant="outline"
                disabled={logsLoading || !!logsError || page <= 1}
                onClick={() => setPage((p) => p - 1)}
              >
                {t("riskControl.text149")}
              </Button>
              <Button
                variant="outline"
                disabled={
                  logsLoading || !!logsError || page * 20 >= (logs?.total ?? 0)
                }
                onClick={() => setPage((p) => p + 1)}
              >
                {t("riskControl.text150")}
              </Button>
            </div>
          </div>
        </Section>
      )}

      {view === "hashes" && (
        <>
          <Section
            title={t("riskControl.text157")}
            description={t("riskControl.text158")}
          >
            <p className="text-sm">
              {t("riskControl.hashCount", { count: status?.hashes ?? 0 })}
            </p>
            <Field label={t("riskControl.text160")}>
              <Input
                className="font-mono"
                value={hash}
                onChange={(e) => setHash(e.target.value)}
                placeholder={t("riskControl.text161")}
              />
            </Field>
            <div className="flex flex-wrap gap-3">
              <Button
                variant="outline"
                disabled={busy || !/^[a-fA-F0-9]{64}$/.test(hash)}
                onClick={async () => {
                  if (await ask(t("riskControl.text162")))
                    void perform(
                      () => api.deleteRiskHash(hash),
                      t("riskControl.text163"),
                    );
                }}
              >
                {t("riskControl.text160")}
              </Button>
              <Button
                variant="outline"
                disabled={busy || !status?.hashes}
                onClick={async () => {
                  if (await ask(t("riskControl.text164")))
                    void perform(
                      () => api.deleteRiskHash(),
                      t("riskControl.text165"),
                    );
                }}
              >
                {t("riskControl.text166")}
              </Button>
            </div>
          </Section>
        </>
      )}
      {view === "test" && (
        <Section
          title={t("riskControl.text167")}
          description={t("riskControl.text168")}
        >
          <textarea
            aria-label={t("riskControl.text169")}
            className={textarea + " min-h-48"}
            value={testText}
            onChange={(e) => setTestText(e.target.value)}
            placeholder={t("riskControl.text170")}
          />
          <div className="flex flex-wrap gap-3">
            {(["keyword", "api"] as const).map((kind) => (
              <Button
                key={kind}
                variant={kind === "api" ? "default" : "outline"}
                disabled={busy || !testText.trim()}
                onClick={() =>
                  void perform(async () => {
                    setTestResult(await api.testRisk(kind, testText));
                  }, t("riskControl.text171"))
                }
              >
                <FlaskConical className="size-4" />
                {kind === "api"
                  ? t("riskControl.text172")
                  : t("riskControl.text173")}
              </Button>
            ))}
          </div>
          {dirty && (
            <p className="text-sm text-amber-600">{t("riskControl.text174")}</p>
          )}
          {testResult && (
            <pre
              aria-live="polite"
              className="overflow-x-auto rounded-xl bg-muted p-4 text-sm"
            >
              {JSON.stringify(testResult, null, 2)}
            </pre>
          )}
        </Section>
      )}
      {view === "prompt" && (
        <Section
          title={t("riskControl.text175")}
          description={t("riskControl.text176")}
        >
          <div className="grid gap-4 md:grid-cols-2">
            {[
              ["overview", t("riskControl.text177"), t("riskControl.text178")],
              ["logs", t("riskControl.text179"), t("riskControl.text180")],
              ["profiles", t("riskControl.text181"), t("riskControl.text182")],
              ["rules", t("riskControl.text183"), t("riskControl.text184")],
            ].map(([v, title, desc]) => (
              <NavLink
                className="rounded-xl border p-5 transition hover:border-primary hover:bg-muted/40"
                key={v}
                to={"/risk-control/prompt/" + v}
              >
                <h4 className="flex items-center gap-2 font-medium">
                  {title}
                  <ExternalLink className="size-4" />
                </h4>
                <p className="mt-2 text-sm text-muted-foreground">{desc}</p>
              </NavLink>
            ))}
          </div>
          <p className="text-xs text-muted-foreground">
            {t("riskControl.text185")}
          </p>
        </Section>
      )}
    </div>
  );
}
