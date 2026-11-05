import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Route, Routes } from "react-router-dom";
import "./index.css";
import { api, type Meta } from "./api";
import { useAsync, useToken } from "./hooks";
import { Layout } from "./Layout";
import { Dashboard } from "./pages/Dashboard";
import { Executions, Integrations, Settings } from "./pages/Other";
import { TaskForm } from "./pages/TaskForm";
import { Tasks } from "./pages/Tasks";
import { TokenGate } from "./pages/TokenGate";
import { EmptyState } from "./ui";

function App() {
  const token = useToken();
  if (!token) return <TokenGate />;
  return <AuthedApp key={token} />;
}

function AuthedApp() {
  const meta = useAsync<Meta>(() => api.meta(), []);
  return (
    <Routes>
      <Route element={<Layout version={meta.status === "ready" ? meta.data.version : undefined} />}>
        <Route index element={<Dashboard />} />
        <Route path="tasks" element={<Tasks />} />
        <Route path="tasks/new" element={<TaskForm />} />
        <Route path="tasks/:id" element={<TaskForm />} />
        <Route path="executions" element={<Executions />} />
        <Route path="integrations" element={<Integrations />} />
        <Route path="settings" element={<Settings />} />
        <Route path="*" element={<EmptyState title="Page not found" body="There is nothing at this address." />} />
      </Route>
    </Routes>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
);
