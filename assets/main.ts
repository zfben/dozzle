import "./main.css";
import { createApp, App as VueApp } from "vue";
import App from "./App.vue";

const app = createApp(App);
Object.values(import.meta.glob<{ install: (app: VueApp) => void }>("./modules/*.ts", { eager: true })).forEach((i) =>
  i.install?.(app),
);

app.mount("#app");

import workerUrl from "./worker.ts?worker&url";

const js = `import ${JSON.stringify(new URL(workerUrl, import.meta.url))}`;
const blob = new Blob([js], { type: "application/javascript" });
const objURL = URL.createObjectURL(blob);
const worker = new Worker(objURL, { type: "module" });
worker.addEventListener("error", (e) => {
  URL.revokeObjectURL(objURL);
});
