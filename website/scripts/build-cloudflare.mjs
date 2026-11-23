import { fileURLToPath } from "node:url";
import {
  cleanBuildOutputDir,
  getWorkerBundleDir,
  writeAssets,
  writeRootConfig,
  writeWorkerConfig,
} from "@cloudflare/build-output-utils";
import { build } from "vite";
import config from "../cloudflare.config.ts";

const root = fileURLToPath(new URL("../", import.meta.url));
const mainModule = "homepage.mjs";
await cleanBuildOutputDir(root);
await build({
  root,
  configFile: false,
  publicDir: false,
  build: {
    lib: { entry: config.worker.entrypoint, formats: ["es"], fileName: () => mainModule },
    outDir: getWorkerBundleDir(root),
  },
});
await writeAssets({ root, sourceDirectory: fileURLToPath(new URL("../dist/client/", import.meta.url)) });
await writeWorkerConfig({
  root,
  config: config.worker,
  manifest: { type: "complete", mainModule, modules: { [mainModule]: { type: "esm" } } },
});
await writeRootConfig({ root, buildContext: { isPreview: false } });
