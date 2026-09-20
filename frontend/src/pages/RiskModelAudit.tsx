import {
  cloneElement,
  useId,
  useState,
  type ReactElement,
  type ReactNode,
} from "react";
import { useTranslation } from "react-i18next";
import {
  Plus,
  ArrowUp,
  ArrowDown,
  Pencil,
  Trash2,
  FlaskConical,
  RefreshCw,
  Save,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Link } from "react-router-dom";
import { Switch } from "@/components/ui/switch";
import { DraftNumberInput } from "@/components/ui/draft-number-input";
import { Card, CardContent } from "@/components/ui/card";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { getErrorMessage } from "../utils/error";
import { api } from "../api";
import { createAuditNodeID } from "../lib/riskControl";
import type {
  AuditNode,
  ModelAuditPolicy,
  RiskConfig,
  RiskConfigView,
} from "../lib/riskControl";

function Field({
  label,
  children,
}: {
  label: string;
  children: ReactElement<{ id?: string }>;
}) {
  const id = useId();
  return (
    <div className="min-w-0 space-y-2">
      <label htmlFor={id} className="block text-sm font-medium">
        {label}
      </label>
      {cloneElement(children, { id })}
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

export default function RiskModelAudit({
  config,
  view,
  onChange,
  onSave,
  saving,
  dirty,
}: {
  config: RiskConfig;
  view: RiskConfigView;
  onChange: (config: RiskConfig) => void;
  onSave: () => Promise<void>;
  saving: boolean;
  dirty: boolean;
}) {
  const { t } = useTranslation();
  const tr = (key: string) => t("riskControl.modelAudit." + key);
  const a = config.model_audit;
  const patch = (value: Partial<ModelAuditPolicy>) =>
    onChange({ ...config, model_audit: { ...a, ...value } });
  const [editing, setEditing] = useState<AuditNode | null>(null);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<Record<string, unknown> | null>(null);
  const [probes, setProbes] = useState<Record<string, boolean>>({});
  const { confirm, confirmDialog } = useConfirmDialog();
  const updateNode = (node: AuditNode) =>
    patch({
      nodes: a.nodes.some((n) => n.id === node.id)
        ? a.nodes.map((n) => (n.id === node.id ? node : n))
        : [...a.nodes, node],
    });
  const editPatch = (value: Partial<AuditNode>) =>
    setEditing((n) => (n ? { ...n, ...value } : n));
  const move = (index: number, offset: number) => {
    const nodes = [...a.nodes];
    [nodes[index], nodes[index + offset]] = [
      nodes[index + offset],
      nodes[index],
    ];
    patch({ nodes });
  };
  async function test(nodeID?: string) {
    setBusy(true);
    setError("");
    setResult(null);
    try {
      const response = await api.testModelAudit(
        a,
        nodeID ? "Hello, have a nice day." : text,
        nodeID,
      );
      setResult(response);
      if (nodeID)
        setProbes((values) => ({
          ...values,
          [nodeID]: response.action !== "error",
        }));
    } catch (e) {
      setError(getErrorMessage(e, tr("testError")));
      if (nodeID) setProbes((values) => ({ ...values, [nodeID]: false }));
    } finally {
      setBusy(false);
    }
  }
  const toggle = (
    label: string,
    checked: boolean,
    onCheckedChange: (v: boolean) => void,
  ) => (
    <label className="flex items-center gap-3 text-sm">
      <Switch checked={checked} onCheckedChange={onCheckedChange} />
      {label}
    </label>
  );
  const textarea =
    "w-full rounded-lg border border-border bg-background p-3 text-sm outline-none focus:ring-2 focus:ring-ring";
  return (
    <div className="space-y-5">
      {confirmDialog}
      <Section title={tr("title")} description={tr("intro")}>
        <div
          role="region"
          aria-label={tr("sharedPolicy")}
          className="space-y-3"
        >
          <p className="text-sm text-muted-foreground">
            {tr("sharedPolicyHint")}
          </p>
          <Link
            className="inline-block text-sm text-primary underline underline-offset-4"
            to="/risk-control/policy"
          >
            {tr("editSharedPolicy")}
          </Link>
        </div>
        <p className="rounded-lg bg-amber-500/10 p-3 text-sm text-amber-700 dark:text-amber-300">
          {tr("activationHint")}
        </p>
      </Section>
      <Section title={tr("pool")} description={tr("priority")}>
        <Button
          variant="outline"
          disabled={a.nodes.length >= 16}
          onClick={() =>
            setEditing({
              id: createAuditNodeID(),
              name: "",
              enabled: true,
              base_url: "https://api.openai.com",
              model: "",
              timeout_ms: 40000,
              max_input_chars: 400000,
              api_key: "",
            })
          }
        >
          <Plus className="size-4" />
          {tr("add")}
        </Button>
        {a.nodes.length === 0 ? (
          <p className="rounded-lg border border-dashed p-8 text-center text-muted-foreground">
            {tr("empty")}
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[820px] text-left text-sm">
              <thead>
                <tr>
                  {["node", "model", "limits", "credential", "actions"].map(
                    (k) => (
                      <th className="p-3" key={k}>
                        {tr(k)}
                      </th>
                    ),
                  )}
                </tr>
              </thead>
              <tbody>
                {a.nodes.map((node, index) => (
                  <tr key={node.id} className="border-t align-top">
                    <td className="p-3">
                      <div className="flex items-center gap-3">
                        <Switch
                          aria-label={t("riskControl.modelAudit.toggleNode", {
                            name: node.name,
                          })}
                          checked={node.enabled}
                          onCheckedChange={(enabled) =>
                            updateNode({ ...node, enabled })
                          }
                        />
                        <div>
                          <strong>{node.name}</strong>
                          <p className="max-w-64 break-all text-xs text-muted-foreground">
                            {node.base_url}
                          </p>
                        </div>
                      </div>
                    </td>
                    <td className="p-3">
                      <p>{node.model}</p>
                      <p className="mt-1 text-xs text-primary">
                        {tr("customPrompt")}
                      </p>
                    </td>
                    <td className="p-3 whitespace-nowrap">
                      {node.timeout_ms} ms
                      <p>
                        {t("riskControl.modelAudit.charLimit", {
                          count: node.max_input_chars,
                        })}
                      </p>
                    </td>
                    <td className="p-3">
                      {(node.api_key || node.has_api_key) && !node.clear_api_key
                        ? tr("configured")
                        : tr("noCredential")}
                      {probes[node.id] !== undefined && (
                        <p
                          role="status"
                          className={
                            probes[node.id]
                              ? "mt-1 text-xs text-emerald-600"
                              : "mt-1 text-xs text-destructive"
                          }
                        >
                          {tr(probes[node.id] ? "probeOk" : "probeFailed")}
                        </p>
                      )}
                    </td>
                    <td className="p-3">
                      <div className="flex flex-wrap gap-2">
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={busy}
                          onClick={() => void test(node.id)}
                        >
                          <FlaskConical className="size-4" />
                          {tr("probe")}
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() =>
                            setEditing({ ...node, api_key: node.api_key ?? "" })
                          }
                        >
                          <Pencil className="size-4" />
                          {tr("edit")}
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          aria-label={tr("up")}
                          disabled={index === 0}
                          onClick={() => move(index, -1)}
                        >
                          <ArrowUp className="size-4" />
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          aria-label={tr("down")}
                          disabled={index === a.nodes.length - 1}
                          onClick={() => move(index, 1)}
                        >
                          <ArrowDown className="size-4" />
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          aria-label={t("riskControl.modelAudit.removeNode", {
                            name: node.name,
                          })}
                          onClick={async () => {
                            if (
                              await confirm({
                                title: tr("remove"),
                                description: tr("removeHint"),
                                tone: "destructive",
                              })
                            )
                              patch({
                                nodes: a.nodes.filter((n) => n.id !== node.id),
                              });
                          }}
                        >
                          <Trash2 className="size-4" />
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {editing && (
          <div
            className="space-y-4 rounded-xl border border-primary/30 p-4"
            role="region"
            aria-label={tr("editNode")}
          >
            <h4 className="font-medium">{tr("editNode")}</h4>
            <div className="grid gap-4 md:grid-cols-2">
              <Field label={tr("name")}>
                <Input
                  value={editing.name}
                  maxLength={100}
                  onChange={(e) => editPatch({ name: e.target.value })}
                />
              </Field>
              <Field label={tr("model")}>
                <Input
                  value={editing.model}
                  maxLength={200}
                  placeholder={tr("modelPlaceholder")}
                  onChange={(e) => editPatch({ model: e.target.value })}
                />
              </Field>
              <Field label={tr("url")}>
                <Input
                  value={editing.base_url}
                  onChange={(e) => editPatch({ base_url: e.target.value })}
                />
              </Field>
              <Field label={tr("apiKey")}>
                <Input
                  type="password"
                  autoComplete="new-password"
                  value={editing.api_key ?? ""}
                  placeholder={tr("keepSecret")}
                  onChange={(e) =>
                    editPatch({ api_key: e.target.value, clear_api_key: false })
                  }
                />
              </Field>
              <Field label={tr("timeout")}>
                <DraftNumberInput
                  value={editing.timeout_ms}
                  min={100}
                  max={120000}
                  onValueChange={(timeout_ms) => editPatch({ timeout_ms })}
                />
              </Field>
              <Field label={tr("chunkLimit")}>
                <DraftNumberInput
                  value={editing.max_input_chars}
                  min={100}
                  max={400000}
                  onValueChange={(max_input_chars) =>
                    editPatch({ max_input_chars })
                  }
                />
              </Field>
            </div>
            <p className="text-xs text-muted-foreground">
              {tr("keepSecretHint")}
            </p>
            {editing.has_api_key &&
              toggle(tr("clearSecret"), !!editing.clear_api_key, (v) =>
                editPatch({ clear_api_key: v, ...(v ? { api_key: "" } : {}) }),
              )}
            <div className="flex gap-3">
              <Button
                disabled={
                  !editing.name.trim() ||
                  !editing.model.trim() ||
                  !editing.base_url.trim()
                }
                onClick={() => {
                  updateNode(editing);
                  setEditing(null);
                }}
              >
                {tr("applyNode")}
              </Button>
              <Button variant="outline" onClick={() => setEditing(null)}>
                {t("common.cancel")}
              </Button>
            </div>
          </div>
        )}
      </Section>
      <Section title={tr("policy")} description={tr("policyHint")}>
        <div className="grid gap-5 md:grid-cols-2">
          <div className="space-y-4">
            {toggle(tr("categorized"), a.categorized, (v) =>
              patch({ categorized: v }),
            )}
            <p className="text-xs text-muted-foreground">
              {tr(a.categorized ? "categoriesHint" : "confidenceHint")}
            </p>
            <div className="grid gap-3 sm:grid-cols-2">
              {view.audit_categories.map((cat) => (
                <label key={cat} className="flex items-center gap-3 text-sm">
                  <Switch
                    disabled={!a.categorized}
                    checked={a.categories.includes(cat)}
                    onCheckedChange={(v) =>
                      patch({
                        categories: v
                          ? [...a.categories, cat]
                          : a.categories.filter((x) => x !== cat),
                      })
                    }
                  />
                  {tr("categories." + cat)}
                </label>
              ))}
            </div>
          </div>
          <div className="space-y-4">
            <Field label={tr("workers")}>
              <DraftNumberInput
                value={config.worker_count}
                min={1}
                max={32}
                onValueChange={(worker_count) =>
                  onChange({ ...config, worker_count })
                }
              />
            </Field>
            <Field label={tr("queue")}>
              <DraftNumberInput
                value={config.queue_size}
                min={1}
                max={100000}
                onValueChange={(queue_size) =>
                  onChange({ ...config, queue_size })
                }
              />
            </Field>
            <p className="text-xs text-muted-foreground">{tr("queueHint")}</p>
            <p className="rounded-lg bg-muted p-3 text-sm">{tr("failOpen")}</p>
            {toggle(tr("storePass"), config.record_non_hits, (v) =>
              onChange({ ...config, record_non_hits: v }),
            )}
          </div>
        </div>
        <div className="grid gap-5 md:grid-cols-2">
          <Field label={tr("blockThreshold")}>
            <DraftNumberInput
              value={a.block_threshold}
              min={0}
              max={1}
              step={0.01}
              onValueChange={(block_threshold) => patch({ block_threshold })}
            />
          </Field>
          <Field label={tr("flagThreshold")}>
            <DraftNumberInput
              value={a.flag_threshold}
              min={0}
              max={1}
              step={0.01}
              onValueChange={(flag_threshold) => patch({ flag_threshold })}
            />
          </Field>
          <Field label={tr("blockStatus")}>
            <DraftNumberInput
              value={config.block_status}
              min={400}
              max={499}
              onValueChange={(block_status) =>
                onChange({ ...config, block_status })
              }
            />
          </Field>
          <Field label={tr("blockMessage")}>
            <Input
              value={config.block_message}
              onChange={(e) =>
                onChange({ ...config, block_message: e.target.value })
              }
            />
          </Field>
        </div>
        <p className="text-xs text-muted-foreground">{tr("thresholdHint")}</p>
      </Section>
      <Section title={tr("customPrompt")} description={tr("promptHint")}>
        <div className="flex flex-wrap gap-3">
          <Button
            variant="outline"
            onClick={async () => {
              if (
                await confirm({
                  title: tr("restore"),
                  description: tr("replacePrompt"),
                })
              )
                patch({
                  system_prompt: view.audit_default_prompt,
                  categorized: false,
                });
            }}
          >
            {tr("restore")}
          </Button>
          <Button
            variant="outline"
            onClick={async () => {
              if (
                await confirm({
                  title: tr("categoryTemplate"),
                  description: tr("replacePrompt"),
                })
              )
                patch({
                  system_prompt: view.audit_category_prompt,
                  categorized: true,
                });
            }}
          >
            {tr("categoryTemplate")}
          </Button>
        </div>
        <textarea
          aria-label={tr("customPrompt")}
          className={textarea + " min-h-72 font-mono"}
          value={a.system_prompt}
          maxLength={20000}
          onChange={(e) => patch({ system_prompt: e.target.value })}
        />
        <p className="text-xs text-muted-foreground">
          {t("riskControl.modelAudit.promptLength", {
            count: Array.from(a.system_prompt).length,
          })}
        </p>
        <div className="flex flex-wrap items-center gap-3">
          <Button disabled={saving || !dirty} onClick={() => void onSave()}>
            <Save className="size-4" />
            {saving ? t("riskControl.text020") : tr("savePrompt")}
          </Button>
          <p className="text-xs text-muted-foreground">
            {tr("savePromptHint")}
          </p>
        </div>
        <pre className="overflow-x-auto rounded-lg bg-muted p-3 text-xs">
          {
            '{"risk":"safe|controversial|unsafe","confidence":0.8,"categories":["violence"],"reason":"..."}'
          }
        </pre>
      </Section>
      <Section title={tr("trial")} description={tr("trialHint")}>
        <textarea
          aria-label={tr("trialInput")}
          className={textarea + " min-h-32"}
          value={text}
          onChange={(e) => setText(e.target.value)}
        />
        <Button
          disabled={busy || !text.trim() || a.nodes.length === 0}
          onClick={() => void test()}
        >
          {busy ? (
            <RefreshCw className="size-4 animate-spin" />
          ) : (
            <FlaskConical className="size-4" />
          )}
          {busy ? tr("testing") : tr("trial")}
        </Button>
        {error && (
          <p role="alert" className="text-destructive">
            {error}
          </p>
        )}
        {result && (
          <pre
            aria-live="polite"
            className="overflow-x-auto rounded-lg bg-muted p-4 text-sm"
          >
            {JSON.stringify(result, null, 2)}
          </pre>
        )}
      </Section>
      <p className="text-sm text-muted-foreground">{tr("saveHint")}</p>
    </div>
  );
}
