import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { ArrowLeft, ShieldCheck } from "lucide-react";
import PromptFilter from "./PromptFilter";
export default function RiskPrompt() {
  const { t } = useTranslation();
  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-muted/30 p-4">
        <div className="flex items-center gap-2 font-medium">
          <ShieldCheck className="size-5 text-primary" />
          {t("riskControl.text186")}
        </div>
        <Link
          to="/risk-control/prompt"
          className="flex items-center gap-2 text-sm text-primary"
        >
          <ArrowLeft className="size-4" />
          {t("riskControl.text187")}
        </Link>
      </div>
      <PromptFilter />
    </div>
  );
}
