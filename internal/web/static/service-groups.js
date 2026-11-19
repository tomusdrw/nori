let drag;
let saving = false;
const status = document.getElementById("group-reorder-status");

function announce(message, error = false) {
  status.textContent = message;
  status.classList.toggle("group-screen-reader", !error);
}

function moveGroup(handle, target, before) {
  if (saving) return;
  const board = handle.closest("#services");
  const name = handle.closest(".dashboard-group").querySelector("h2 button span:last-child").textContent;
  saving = true;
  announce("");
  const onResult = event => {
    if (event.detail.successful) announce(name + " moved.");
    else announce("Could not reorder groups. Try again.", true);
  };
  status.addEventListener("htmx:afterRequest", onResult);
  window.htmx.ajax("POST", handle.dataset.groupMove, {
    source: status,
    target: board,
    swap: "outerHTML",
    values: { target: target.dataset.groupId, before: String(before) },
  }).catch(() => announce("Could not reorder groups. Try again.", true)).finally(() => {
    saving = false;
    status.removeEventListener("htmx:afterRequest", onResult);
  });
}

document.addEventListener("keydown", event => {
  const handle = event.target.closest("[data-group-move]");
  if (!handle || !["ArrowUp", "ArrowDown"].includes(event.key)) return;
  event.preventDefault();
  const group = handle.closest(".dashboard-group");
  const before = event.key === "ArrowUp";
  const target = before ? group.previousElementSibling : group.nextElementSibling;
  if (target?.matches(".dashboard-group")) moveGroup(handle, target, before);
});

document.addEventListener("pointerdown", event => {
  const handle = event.target.closest("[data-group-move]");
  if (!handle || event.button !== 0 || drag || saving) return;
  const source = handle.closest(".dashboard-group");
  drag = { source, handle, startY: event.clientY, pointer: event.pointerId, target: null, before: false };
  handle.setPointerCapture(event.pointerId);
});

document.addEventListener("pointermove", event => {
  if (!drag || drag.pointer !== event.pointerId) return;
  if (!drag.source.classList.contains("is-moving") && Math.abs(event.clientY - drag.startY) < 6) return;
  drag.source.classList.add("is-moving");
  drag.target?.removeAttribute("data-group-drop");
  const target = document.elementFromPoint(event.clientX, event.clientY)?.closest(".dashboard-group");
  drag.target = target && target !== drag.source ? target : null;
  if (!drag.target) return;
  const header = drag.target.querySelector("header").getBoundingClientRect();
  drag.before = event.clientY < header.top + header.height / 2;
  drag.target.dataset.groupDrop = drag.before ? "before" : "after";
});

function finishDrag(event) {
  if (!drag || event.pointerId !== drag.pointer) return;
  const { source, handle, target, before } = drag;
  drag = null;
  source.classList.remove("is-moving");
  target?.removeAttribute("data-group-drop");
  if (target && event.type === "pointerup") moveGroup(handle, target, before);
}

document.addEventListener("pointerup", finishDrag);
document.addEventListener("pointercancel", finishDrag);
document.addEventListener("lostpointercapture", finishDrag);

// A refresh must not replace the DOM holding pointer capture, or race a move.
document.addEventListener("htmx:beforeRequest", event => {
  const { requestConfig } = event.detail;
  if ((drag || saving) && requestConfig.path === "/partials/services") event.preventDefault();
});
document.addEventListener("htmx:beforeSwap", event => {
  if (event.detail.target.id !== "services") return;
  if (drag || (saving && event.detail.requestConfig.path === "/partials/services")) event.preventDefault();
});
