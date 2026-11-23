import Negotiator from "negotiator";
import { recordMarkdownRequest } from "./markdown-analytics.js";

const markdown = "text/markdown; charset=utf-8";
const html = "text/html; charset=utf-8";

function prefersMarkdown(accept) {
  const negotiator = new Negotiator({ headers: { accept } });
  // Wildcard-only clients, including curl and some browsers, get the homepage.
  return negotiator.mediaTypes().some((type) => type.toLowerCase() === "text/markdown")
    && negotiator.mediaType([html, markdown]) === markdown;
}

export default {
  async fetch(request, env, ctx) {
    const { pathname, hostname } = new URL(request.url);
    const isHomepage = pathname === "/";
    const isGuideFile = pathname === "/llms-full.txt" || pathname === "/llms.txt";
    if ((!isHomepage && !isGuideFile) || !["GET", "HEAD"].includes(request.method)) {
      return env.ASSETS.fetch(request);
    }

    const serveMarkdown = isHomepage && prefersMarkdown(request.headers.get("Accept") ?? "");
    const assetRequest = serveMarkdown
      ? new Request(new URL("/llms-full.txt", request.url), request)
      : request;
    const asset = await env.ASSETS.fetch(assetRequest);
    const served = asset.ok || asset.status === 304;
    if ((serveMarkdown || isGuideFile) && served && hostname === "degitide.trade") {
      ctx.waitUntil(recordMarkdownRequest(request, pathname, asset.status));
    }
    if (!isHomepage) return asset;

    const response = new Response(asset.body, asset);
    response.headers.append("Vary", "Accept");
    if (serveMarkdown && served) {
      response.headers.set("Content-Type", markdown);
    }
    return response;
  },
};
