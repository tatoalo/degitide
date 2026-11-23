const agentNames = [
  "ChatGPT-User", "OAI-SearchBot", "GPTBot",
  "Claude-User", "Claude-SearchBot", "ClaudeBot",
  "Perplexity-User", "PerplexityBot", "Googlebot", "bingbot",
  "DuckAssistBot", "Applebot", "Bytespider", "CCBot",
  "curl", "Wget", "python-requests", "Python-urllib", "Go-http-client",
];
const agents = agentNames.map(name => [name, new RegExp(`\\b${name}\\b`, "i")]);

export async function recordMarkdownRequest(request, path, status) {
  const userAgent = request.headers.get("User-Agent") ?? "";
  const agent = agents.find(([, pattern]) => pattern.test(userAgent))?.[0] ?? "Unknown";
  try {
    const result = await fetch("https://umami.tatoalo.org/api/logs", {
      method: "POST",
      headers: { "Content-Type": "application/json", "User-Agent": "Degitide/1.0 (guide analytics)" },
      redirect: "manual",
      signal: AbortSignal.timeout(5000),
      body: JSON.stringify({
        type: "event",
        payload: {
          website: "b9783b90-fc6a-40a8-bee4-82a8d57ae610",
          hostname: "degitide.trade",
          url: path,
          name: "markdown_request",
          data: { agent, path, method: request.method, status },
        },
      }),
    });
    if (!result.ok) throw new Error(`Collector returned HTTP ${result.status}`);
    // Umami can reply with HTTP 200 when it filters a request. Check its receipt.
    const receipt = await result.json();
    if (!receipt?.sessionId && !receipt?.cache) throw new Error("Collector did not acknowledge the event");
  } catch (error) {
    console.warn({ event: "markdown_analytics_failed", path, message: error.message });
  }
}
