import ReactDOM from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ToastProvider } from "../src/components/ToastProvider";
import RiskControl from "../src/pages/RiskControl";
import "../src/i18n";
import "../src/index.css";

ReactDOM.createRoot(document.getElementById("root")!).render(
  <MemoryRouter initialEntries={["/risk-control/model-audit"]}>
    <ToastProvider>
      <Routes>
        <Route path="/risk-control/:view" element={<RiskControl />} />
      </Routes>
    </ToastProvider>
  </MemoryRouter>,
);
