import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

const page = document.querySelector("[data-terminal-page]");
const host = page?.querySelector("[data-terminal]");

if (page && host) {
  const status = page.querySelector("[data-terminal-status]");
  const statusLabel = page.querySelector("[data-terminal-status-label]");
  const reconnectButton = page.querySelector("[data-terminal-reconnect]");
  const clearButton = page.querySelector("[data-terminal-clear]");
  const terminal = new Terminal({
    cursorBlink: true,
    cursorStyle: "bar",
    fontFamily: '"SFMono-Regular", "Cascadia Code", Consolas, monospace',
    fontSize: 13,
    lineHeight: 1.35,
    scrollback: 10000,
    allowTransparency: true,
    theme: {
      background: "#070b14",
      foreground: "#c6cede",
      cursor: "#9b8cff",
      cursorAccent: "#070b14",
      selectionBackground: "#3b3566",
      black: "#111827",
      red: "#f97066",
      green: "#32d583",
      yellow: "#fdb022",
      blue: "#7c8df2",
      magenta: "#b692f6",
      cyan: "#28c9d8",
      white: "#dbe4f4",
      brightBlack: "#667085",
      brightRed: "#ff9c94",
      brightGreen: "#72e5a7",
      brightYellow: "#fbcf75",
      brightBlue: "#9bafff",
      brightMagenta: "#d0b5ff",
      brightCyan: "#78dce5",
      brightWhite: "#f8faff",
    },
  });
  const fit = new FitAddon();
  terminal.loadAddon(fit);
  terminal.open(host);

  let socket = null;
  let reconnectTimer = null;
  let reconnectDelay = 1000;

  const setStatus = (state, label) => {
    status.dataset.state = state;
    statusLabel.textContent = label;
  };

  const send = (message) => {
    if (socket?.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify(message));
    }
  };

  const sendSize = () => send({ type: "resize", rows: terminal.rows, cols: terminal.cols });

  const scheduleReconnect = () => {
    if (reconnectTimer || document.visibilityState === "hidden") return;
    setStatus("disconnected", `Reconnecting in ${Math.ceil(reconnectDelay / 1000)}s`);
    reconnectTimer = window.setTimeout(() => {
      reconnectTimer = null;
      connect();
    }, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, 10000);
  };

  const connect = () => {
    if (socket && (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING)) return;
    setStatus("connecting", "Connecting");
    const scheme = window.location.protocol === "https:" ? "wss" : "ws";
    const connection = new WebSocket(`${scheme}://${window.location.host}/terminal/ws`);
    socket = connection;
    connection.binaryType = "arraybuffer";

    connection.addEventListener("open", () => {
      if (socket !== connection) return;
      reconnectDelay = 1000;
      setStatus("connected", "Connected");
      fit.fit();
      sendSize();
      terminal.focus();
    });
    connection.addEventListener("message", (event) => {
      if (socket !== connection) return;
      if (event.data instanceof ArrayBuffer) terminal.write(new Uint8Array(event.data));
      else terminal.write(event.data);
    });
    connection.addEventListener("close", () => {
      if (socket !== connection) return;
      socket = null;
      scheduleReconnect();
    });
    connection.addEventListener("error", () => {
      if (socket === connection) setStatus("disconnected", "Connection lost");
    });
  };

  terminal.onData((data) => send({ type: "input", data }));
  terminal.onResize(({ rows, cols }) => send({ type: "resize", rows, cols }));

  const resizeObserver = new ResizeObserver(() => {
    window.requestAnimationFrame(() => {
      fit.fit();
      sendSize();
    });
  });
  resizeObserver.observe(host);

  reconnectButton?.addEventListener("click", () => {
    if (reconnectTimer) window.clearTimeout(reconnectTimer);
    reconnectTimer = null;
    const previous = socket;
    socket = null;
    previous?.close();
    reconnectDelay = 1000;
    connect();
  });
  clearButton?.addEventListener("click", () => {
    terminal.clear();
    terminal.focus();
  });
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && !socket) connect();
  });
  window.setInterval(() => send({ type: "ping" }), 20000);

  fit.fit();
  connect();
}
