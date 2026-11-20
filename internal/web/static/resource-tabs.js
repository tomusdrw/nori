const tabList = document.querySelector(".resource-tabs");
const tabs = [...tabList.querySelectorAll("a")];
const panels = tabs.map(tab => document.getElementById(tab.hash.slice(1)));

tabList.setAttribute("role", "tablist");
tabs.forEach((tab, index) => {
  const panel = panels[index];
  tab.id = panel.id + "-tab";
  tab.setAttribute("role", "tab");
  tab.setAttribute("aria-controls", panel.id);
  panel.setAttribute("role", "tabpanel");
  panel.setAttribute("aria-labelledby", tab.id);
  panel.tabIndex = 0;
});

function selectTab(selected) {
  tabs.forEach((tab, index) => {
    const active = tab === selected;
    tab.setAttribute("aria-selected", String(active));
    tab.tabIndex = active ? 0 : -1;
    panels[index].hidden = !active;
  });
}

function selectFromHash() {
  selectTab(tabs.find(tab => tab.hash === location.hash) ?? tabs[0]);
}

function openTab(tab) {
  if (location.hash !== tab.hash) history.pushState(null, "", tab.hash);
  selectTab(tab);
}

tabs.forEach((tab, index) => {
  tab.addEventListener("click", event => {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    openTab(tab);
  });

  tab.addEventListener("keydown", event => {
    let next;
    switch (event.key) {
      case "ArrowLeft": next = tabs[(index - 1 + tabs.length) % tabs.length]; break;
      case "ArrowRight": next = tabs[(index + 1) % tabs.length]; break;
      case "Home": next = tabs[0]; break;
      case "End": next = tabs.at(-1); break;
      case " ": next = tab; break;
      default: return;
    }
    event.preventDefault();
    openTab(next);
    next.focus();
  });
});

window.addEventListener("hashchange", selectFromHash);
selectFromHash();
