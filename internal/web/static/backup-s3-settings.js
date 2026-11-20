const s3Form = document.getElementById("s3-settings-form");
if (s3Form) {
  const error = document.getElementById("s3-save-error");
  const success = document.getElementById("s3-save-success");
  const submit = s3Form.querySelector('button[type="submit"]');
  let invalidField = s3Form.querySelector('[aria-invalid="true"]');

  function showError(message, fieldName) {
    error.textContent = message;
    error.hidden = false;
    invalidField = fieldName ? s3Form.elements.namedItem(fieldName) : null;
    if (invalidField) {
      invalidField.setAttribute("aria-invalid", "true");
      invalidField.setAttribute("aria-describedby", "s3-help-" + fieldName + " s3-save-error");
    }
    (invalidField || error).focus();
  }

  s3Form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (submit.disabled) return;

    const body = new URLSearchParams(new FormData(s3Form));
    const label = submit.textContent;
    submit.disabled = true;
    submit.textContent = "Saving…";
    error.hidden = true;
    if (success) success.hidden = true;
    if (invalidField) {
      invalidField.removeAttribute("aria-invalid");
      invalidField.setAttribute("aria-describedby", "s3-help-" + invalidField.name);
      invalidField = null;
    }

    try {
      const response = await fetch(s3Form.action, {
        method: "POST",
        headers: { Accept: "application/json", "X-CSRF-Token": body.get("csrf_token") },
        body,
      });
      if (response.redirected || response.status === 403) {
        showError("Your session may have expired. Keep this tab open to copy your entries before signing in again.");
        return;
      }
      const result = await response.json();
      if (response.ok && result.location) {
        const next = new URL(result.location, window.location.href);
        if (next.pathname === window.location.pathname && next.search === window.location.search) {
          window.location.replace(next.href);
          window.location.reload();
        } else {
          window.location.assign(next.href);
        }
      } else {
        showError(result.message || "Could not save S3 settings. Try again.", result.field);
      }
    } catch {
      showError("Could not confirm the save. Check your connection and try again.");
    } finally {
      submit.disabled = false;
      submit.textContent = label;
    }
  });
}
