import assert from "node:assert/strict";
import test, { afterEach, beforeEach, mock } from "node:test";
import worker from "../worker/homepage.js";

const origin = "https://degitide.trade";
const markdown = "text/markdown; charset=utf-8";

beforeEach(() => mock.method(globalThis, "fetch", async () => Response.json({ sessionId: "test" })));
afterEach(() => mock.restoreAll());

async function fetchHomepage(accept, { method = "GET", path = "/", status = 200, host = origin, userAgent } = {}) {
  const headers = { "If-None-Match": '"test"', "User-Agent": userAgent ?? "curl/8.7.1" };
  if (accept !== undefined) headers.Accept = accept;
  let assetRequest;
  const pending = [];
  const response = await worker.fetch(new Request(host + path, { method, headers }), {
    ASSETS: {
      fetch(request) {
        assetRequest = request;
        const isGuide = new URL(request.url).pathname === "/llms-full.txt";
        const body = method === "HEAD" || status === 304 ? null : (isGuide ? "# Full guide" : "<html>");
        return new Response(body, {
          status,
          headers: { "Content-Type": isGuide ? "text/plain" : "text/html", Vary: "Accept-Encoding", ETag: '"test"' },
        });
      },
    },
  }, { waitUntil: promise => pending.push(promise) });
  return { assetRequest, response, pending };
}

test("serves the full guide at the root when Markdown is explicitly preferred", async () => {
  for (const accept of ["text/markdown", "TEXT/MARKDOWN; charset=UTF-8", "text/markdown, */*", "text/html;q=0.6, text/markdown;q=0.9"]) {
    const { assetRequest, response } = await fetchHomepage(accept, { path: "/?ref=test" });
    assert.equal(assetRequest.url, origin + "/llms-full.txt");
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("Content-Type"), markdown);
    assert.equal(response.headers.get("Vary"), "Accept-Encoding, Accept");
    assert.equal(await response.text(), "# Full guide");
  }
});

test("keeps the browser homepage for ordinary requests and HTML preferences", async () => {
  for (const accept of [undefined, "*/*", "text/*", "text/html,application/xhtml+xml,*/*;q=0.8", "text/markdown;q=0, */*", "text/markdown;q=0.5, text/html", "text/markdown-extra"]) {
    const { assetRequest, response } = await fetchHomepage(accept);
    assert.equal(assetRequest.url, origin + "/");
    assert.equal(response.headers.get("Content-Type"), "text/html");
    assert.equal(response.headers.get("Vary"), "Accept-Encoding, Accept");
    assert.equal(await response.text(), "<html>");
  }
});

test("preserves HEAD and conditional requests for the selected representation", async () => {
  for (const status of [200, 304]) {
    const { assetRequest, response } = await fetchHomepage("text/markdown", { method: "HEAD", status });
    assert.equal(assetRequest.method, "HEAD");
    assert.equal(assetRequest.headers.get("If-None-Match"), '"test"');
    assert.equal(response.status, status);
    assert.equal(response.headers.get("ETag"), '"test"');
    assert.equal(response.headers.get("Content-Type"), markdown);
    assert.equal(response.body, null);
  }
});

test("passes through other paths and non-read methods", async () => {
  for (const options of [{ path: "/llms-full.txt" }, { path: "/missing" }, { method: "POST" }]) {
    const { assetRequest, response } = await fetchHomepage("text/markdown", options);
    assert.equal(assetRequest.url, origin + (options.path ?? "/"));
    assert.equal(assetRequest.method, options.method ?? "GET");
    assert.equal(response.headers.get("Vary"), "Accept-Encoding");
  }
});

test("records one event for each guide request with its declared client and a query-free path", async () => {
  for (const [path, method, status, userAgent, agent] of [
    ["/?ref=private", "GET", 200, "Mozilla/5.0 (compatible; ChatGPT-User/1.0)", "ChatGPT-User"],
    ["/llms-full.txt", "GET", 206, "Mozilla/5.0 (compatible; claudebot/1.0)", "ClaudeBot"],
    ["/llms.txt", "HEAD", 304, "custom-client/1.2", "Unknown"],
  ]) {
    const { pending } = await fetchHomepage(path.startsWith("/?") ? "text/markdown" : "*/*", { path, method, status, userAgent });
    assert.equal(pending.length, 1);
    await Promise.all(pending);
    const [url, options] = fetch.mock.calls.at(-1).arguments;
    assert.equal(url, "https://umami.tatoalo.org/api/logs");
    assert.equal(options.method, "POST");
    assert.notEqual(options.headers["User-Agent"], userAgent);
    const cleanPath = new URL(path, origin).pathname;
    assert.deepEqual(JSON.parse(options.body), {
      type: "event",
      payload: {
        website: "b9783b90-fc6a-40a8-bee4-82a8d57ae610", hostname: "degitide.trade",
        url: cleanPath, name: "markdown_request", data: { agent, path: cleanPath, method, status },
      },
    });
  }
  assert.equal(fetch.mock.callCount(), 3);
});

test("skips HTML, other paths, write requests, missing guides and preview hosts", async () => {
  for (const [accept, options] of [
    ["text/html", {}], ["*/*", {}], ["text/markdown", { method: "POST" }],
    ["text/markdown", { path: "/robots.txt" }], ["text/markdown", { path: "/llms.txt", status: 404 }],
    ["text/markdown", { host: "https://preview.example" }],
  ]) {
    const { pending } = await fetchHomepage(accept, options);
    assert.equal(pending.length, 0);
  }
  assert.equal(fetch.mock.callCount(), 0);
});

test("serves the guide before analytics completes and handles a filtered event", async () => {
  const collector = Promise.withResolvers();
  fetch.mock.mockImplementation(() => collector.promise);
  const warnings = mock.method(console, "warn", () => {});
  const { response, pending } = await fetchHomepage("text/markdown");
  assert.equal(await response.text(), "# Full guide");
  collector.resolve(Response.json({ beep: "boop" }));
  await Promise.all(pending);
  assert.equal(warnings.mock.calls[0].arguments[0].event, "markdown_analytics_failed");
});
