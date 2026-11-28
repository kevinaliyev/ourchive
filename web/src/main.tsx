import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./styles/tokens.css";
import "./styles/app.css";
import { App } from "./App";
import { ClientRoot } from "./components/ClientChrome";
import { initPlatform, isClient } from "./platform";

if (!("switch" in HTMLInputElement.prototype)) {
  void import("input-switch-polyfill");
}

void initPlatform().then(() => {
  createRoot(document.getElementById("root")!).render(
    <StrictMode>{isClient ? <ClientRoot renderApp={(key) => <App key={key} />} /> : <App />}</StrictMode>,
  );
});
