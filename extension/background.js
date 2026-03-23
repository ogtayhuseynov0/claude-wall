const API = "http://127.0.0.1:7685";

// Fetch panes and build context menus
async function refreshMenus() {
  await chrome.contextMenus.removeAll();

  // ── Selection context menu ──
  chrome.contextMenus.create({
    id: "claude-wall-selection",
    title: "Send selection to Agent",
    contexts: ["selection"],
  });

  // ── Page context menu (works even when right-click is hijacked) ──
  chrome.contextMenus.create({
    id: "claude-wall-page",
    title: "Send page text to Agent",
    contexts: ["page", "frame"],
  });

  try {
    const res = await fetch(`${API}/api/panes`);
    const panes = await res.json();

    for (const pane of panes) {
      const label = `${pane.session} · ${pane.dirName} (${pane.branch})`;

      // Selection submenu
      chrome.contextMenus.create({
        id: `sel-${pane.target}`,
        parentId: "claude-wall-selection",
        title: label,
        contexts: ["selection"],
      });

      // Page text submenu
      chrome.contextMenus.create({
        id: `page-${pane.target}`,
        parentId: "claude-wall-page",
        title: label,
        contexts: ["page", "frame"],
      });
    }

    if (panes.length > 1) {
      chrome.contextMenus.create({
        id: "sel-all",
        parentId: "claude-wall-selection",
        title: "📢 Send to ALL agents",
        contexts: ["selection"],
      });
      chrome.contextMenus.create({
        id: "page-all",
        parentId: "claude-wall-page",
        title: "📢 Send to ALL agents",
        contexts: ["page", "frame"],
      });
    }

    await chrome.storage.local.set({ panes });
  } catch (e) {
    for (const parent of ["claude-wall-selection", "claude-wall-page"]) {
      chrome.contextMenus.create({
        id: `error-${parent}`,
        parentId: parent,
        title: "⚠ Dashboard not running",
        contexts: ["all"],
        enabled: false,
      });
    }
  }
}

async function sendToPane(target, text) {
  try {
    await fetch(`${API}/api/send/${target}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text }),
    });
    return true;
  } catch (e) {
    return false;
  }
}

async function sendToAll(text) {
  const { panes } = await chrome.storage.local.get("panes");
  if (panes) {
    for (const pane of panes) {
      await sendToPane(pane.target, text);
    }
  }
}

// Extract visible text from page via content script
async function getPageText(tabId) {
  try {
    const results = await chrome.scripting.executeScript({
      target: { tabId },
      func: () => {
        // Get visible text content, cleaned up
        const body = document.body.innerText || document.body.textContent || '';
        const title = document.title || '';
        const url = location.href;
        // Limit to ~4000 chars to avoid overwhelming the agent
        const text = body.substring(0, 4000);
        return `[Page: ${title}]\n[URL: ${url}]\n\n${text}`;
      },
    });
    return results[0]?.result || '';
  } catch (e) {
    return '';
  }
}

// Handle menu clicks
chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  // ── Selection sends ──
  if (info.menuItemId.startsWith("sel-")) {
    const text = info.selectionText;
    if (!text) return;

    if (info.menuItemId === "sel-all") {
      await sendToAll(text);
    } else {
      const target = info.menuItemId.replace("sel-", "");
      await sendToPane(target, text);
    }
    return;
  }

  // ── Page text sends ──
  if (info.menuItemId.startsWith("page-")) {
    const text = await getPageText(tab.id);
    if (!text) return;

    if (info.menuItemId === "page-all") {
      await sendToAll(text);
    } else {
      const target = info.menuItemId.replace("page-", "");
      await sendToPane(target, text);
    }
    return;
  }
});

// Need scripting permission for page text extraction
chrome.runtime.onInstalled.addListener(refreshMenus);
setInterval(refreshMenus, 30000);
refreshMenus();
