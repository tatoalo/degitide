import { useRef, useState } from "react";
import { Check, Copy } from "@phosphor-icons/react";
import promptText from "../public/setup-prompt.txt?raw";

const prompt = promptText.trim();
const encoded = encodeURIComponent(prompt);
const agents = [
  ["Codex", "codex", `codex://threads/new?prompt=${encoded}`],
  ["Claude Code", "claude-code", `claude-cli://open?repo=tatoalo%2Fdegitide&q=${encoded}`],
  ["Cursor", "cursor", `https://cursor.com/link/prompt?text=${encoded}`],
];

export function AgentSetup() {
  const promptField = useRef(null);
  const rawDetails = useRef(null);
  const [showRaw, setShowRaw] = useState(false);
  const [copyStatus, setCopyStatus] = useState("idle");

  async function copyPrompt() {
    try {
      await navigator.clipboard.writeText(prompt);
      setCopyStatus("copied");
    } catch {
      rawDetails.current.open = true;
      promptField.current.focus();
      promptField.current.select();
      setCopyStatus("manual");
    }
  }

  return (
    <section className="agent-setup" id="agent-setup" aria-labelledby="agent-setup-title">
      <div className="agent-heading">
        <h2 id="agent-setup-title">Set up with an agent</h2>
        <button type="button" onClick={copyPrompt} aria-label="Copy setup prompt">
          {copyStatus === "copied" ? <Check size={16} /> : <Copy size={16} />}
          {copyStatus === "copied" ? "Copied" : "Copy prompt"}
        </button>
      </div>
      <div className="agent-choices" role="group" aria-label="Open the setup prompt in an agent">
        {agents.map(([name, icon, href]) => (
          <a key={icon} href={href} aria-label={`Open setup prompt in ${name}`}>
            <img src={`/assets/agents/${icon}.svg`} alt="" width="20" height="20" />
            {name}
          </a>
        ))}
      </div>
      <details
        ref={rawDetails}
        className="agent-raw"
        onToggle={(event) => setShowRaw(event.currentTarget.open)}
      >
        <summary>{showRaw ? "Hide raw prompt" : "Show raw prompt"}</summary>
        <textarea
          className="agent-prompt"
          aria-label="Degitide setup prompt"
          ref={promptField}
          value={prompt}
          readOnly
          spellCheck={false}
        />
      </details>
      <span role="status" className={copyStatus === "manual" ? "agent-copy-help" : "agent-sr-only"}>
        {copyStatus === "manual" && "Prompt selected. Copy it and paste it into your agent."}
        {copyStatus === "copied" && "Setup prompt copied."}
      </span>
    </section>
  );
}
