import { basicSetup } from "codemirror";
import { EditorState } from "@codemirror/state";
import { EditorView, keymap, placeholder } from "@codemirror/view";
import { StreamLanguage } from "@codemirror/language";
import { shell } from "@codemirror/legacy-modes/mode/shell";
import { linter, lintGutter } from "@codemirror/lint";
import { oneDark } from "@codemirror/theme-one-dark";

const dotenv = StreamLanguage.define({
  token(stream) {
    if (stream.sol()) {
      if (stream.match(/^\s*#/)) {
        stream.skipToEnd();
        return "comment";
      }
      if (stream.match(/^\s*(?:export\s+)?[A-Za-z_][A-Za-z0-9_]*(?=\s*=)/)) {
        return "definition(variableName)";
      }
    }
    if (stream.eatSpace()) return null;
    if (stream.eat("=")) return "operator";
    if (stream.peek() === "#") {
      stream.skipToEnd();
      return "comment";
    }
    const quote = stream.peek();
    if (quote === '"' || quote === "'") {
      stream.next();
      let escaped = false;
      while (!stream.eol()) {
        const char = stream.next();
        if (char === quote && !escaped) break;
        escaped = char === "\\" && !escaped;
        if (char !== "\\") escaped = false;
      }
      return "string";
    }
    stream.skipToEnd();
    return "string";
  },
});

function validationStatus(kind, state, message) {
  const element = document.querySelector(`[data-validation-status="${kind}"]`);
  if (!element) return;
  element.classList.remove("is-valid", "is-invalid", "is-checking");
  element.classList.add(`is-${state}`);
  const label = element.querySelector("[data-validation-message]");
  if (label) label.textContent = message;
}

async function validate(kind, view) {
  validationStatus(kind, "checking", kind === "bash" ? "Checking Bash…" : "Checking .env…");
  const token = document.querySelector('meta[name="csrf-token"]')?.content || "";
  const body = new URLSearchParams({ kind, content: view.state.doc.toString() });
  try {
    const response = await fetch("/validate/editor", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "X-CSRF-Token": token,
      },
      body,
    });
    if (!response.ok) throw new Error(`Validation failed (${response.status})`);
    const result = await response.json();
    if (result.valid) {
      validationStatus(kind, "valid", kind === "bash" ? "Bash syntax valid" : ".env syntax valid");
      return [];
    }
    validationStatus(kind, "invalid", result.message || "Invalid syntax");
    const lineNumber = Math.max(1, Math.min(result.line || 1, view.state.doc.lines));
    const line = view.state.doc.line(lineNumber);
    return [{
      from: line.from,
      to: Math.max(line.from, line.to),
      severity: "error",
      message: result.message || "Invalid syntax",
    }];
  } catch (error) {
    validationStatus(kind, "invalid", "Could not verify syntax");
    return [{ from: 0, to: 0, severity: "warning", message: error.message }];
  }
}

function setupEditor(textarea) {
  const kind = textarea.dataset.editor;
  const form = textarea.closest("form");
  const language = kind === "bash" ? StreamLanguage.define(shell) : dotenv;
  const saveKeymap = keymap.of([{
    key: "Mod-s",
    preventDefault: true,
    run: () => {
      form?.requestSubmit();
      return true;
    },
  }]);
  const state = EditorState.create({
    doc: textarea.value,
    extensions: [
      basicSetup,
      language,
      oneDark,
      lintGutter(),
      linter((view) => validate(kind, view), { delay: 450 }),
      placeholder(textarea.placeholder || ""),
      EditorView.lineWrapping,
      saveKeymap,
      EditorView.updateListener.of((update) => {
        if (update.docChanged) {
          textarea.value = update.state.doc.toString();
          validationStatus(kind, "checking", "Checking syntax…");
        }
      }),
      EditorView.theme({
        "&": { height: kind === "bash" ? "390px" : "300px" },
        ".cm-scroller": { overflow: "auto", fontFamily: "var(--font-mono)" },
      }),
    ],
  });
  const host = document.createElement("div");
  host.className = `editor-host editor-host-${kind}`;
  textarea.hidden = true;
  textarea.insertAdjacentElement("afterend", host);
  const view = new EditorView({ state, parent: host });
  form?.addEventListener("submit", () => {
    textarea.value = view.state.doc.toString();
  });
}

function setupPolicyField() {
  const select = document.querySelector("[data-policy-select]");
  const field = document.querySelector("[data-cron-field]");
  if (!select || !field) return;
  const update = () => {
    const scheduled = select.value === "scheduled";
    field.classList.toggle("is-muted", !scheduled);
    field.querySelector("input").readOnly = !scheduled;
  };
  select.addEventListener("change", update);
  update();
}

document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("textarea[data-editor]").forEach(setupEditor);
  setupPolicyField();
});
