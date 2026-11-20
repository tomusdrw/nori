function showIcon(img) {
  if (img.naturalWidth > 0) img.setAttribute("data-loaded", "");
}

// Listen on the document so refreshed service cards also use the fallback
// initial until the image is available.
document.addEventListener("load", event => {
  if (event.target.matches?.(".service-avatar img")) showIcon(event.target);
}, true);

document.querySelectorAll(".service-avatar img").forEach(showIcon);
