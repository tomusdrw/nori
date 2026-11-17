const form = document.querySelector('[data-deployment-form]');
const mode = form?.querySelector('[data-deployment-mode]');
const image = form?.querySelector('[data-watched-image]');
const imageField = form?.querySelector('[data-watched-image-field]');
const policyField = form?.querySelector('[data-policy-field]');
const policySelect = form?.querySelector('[data-policy-select]');
const manualPolicy = form?.querySelector('[data-manual-policy]');
const cronField = form?.querySelector('[data-cron-field]');
const manualScriptHelp = form?.querySelector('[data-manual-script-help]');
const imageScriptHelp = form?.querySelector('[data-image-script-help]');

function syncMode() {
  if (!form || !mode) return;
  for (const panel of form.querySelectorAll('[data-deployment-mode-panel]')) {
    panel.hidden = !panel.dataset.deploymentModePanel.split(',').includes(mode.value);
	for (const control of panel.querySelectorAll('input, select, textarea, button')) {
		control.disabled = panel.hidden;
	}
  }
  const manualScript = mode.value === 'manual_script';
  if (manualScriptHelp) manualScriptHelp.hidden = !manualScript;
  if (imageScriptHelp) imageScriptHelp.hidden = manualScript;
  if (policyField) policyField.hidden = manualScript;
  if (policySelect) policySelect.disabled = manualScript;
  if (manualPolicy) manualPolicy.disabled = !manualScript;
  if (cronField) cronField.hidden = manualScript;
  if (imageField) imageField.hidden = manualScript;
  if (image) {
    image.disabled = manualScript;
    image.required = !manualScript;
  }
}

mode?.addEventListener('change', syncMode);
syncMode();
