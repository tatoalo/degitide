export default {
  worker: {
    name: "degitide-site",
    compatibilityDate: "2026-11-23",
    entrypoint: "./worker/homepage.js",
    env: { ASSETS: { type: "assets" } },
    assets: { runWorkerFirst: ["/", "/llms.txt", "/llms-full.txt"] },
    domains: ["degitide.trade"],
    workersDev: false,
    previewUrls: false,
    observability: {
      enabled: true,
      redactQueryString: true,
      traces: { enabled: true },
    },
  },
};
