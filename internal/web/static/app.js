// Small client-side helpers: selection count, chip filters, toasts.
(() => {
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

  const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;

  // The selection lives here, not in the checkboxes: the chat list is re-rendered
  // whenever a filter changes, and picks that are filtered out of view must survive.
  const selected = new Map(); // key -> { kind, owner }
  const boxes = () => $$('#bulk input[name="keys"]');

  function record(b) {
    if (b.checked) selected.set(b.value, { kind: b.dataset.kind, owner: b.dataset.owner === "true" });
    else selected.delete(b.value);
  }

  function clearSelection() {
    selected.clear();
    boxes().forEach((b) => (b.checked = false));
    updateBulkBar();
  }

  function updateBulkBar() {
    const bar = $("#bulkbar");
    if (!bar) return;
    const shown = boxes();
    const on = shown.filter((b) => b.checked).length;
    const picked = [...selected.values()];
    const n = picked.length;
    bar.hidden = n === 0;
    $("[data-count]", bar).textContent = n;

    const count = (kind) => picked.filter((p) => p.kind === kind).length;
    const ch = count("channel"), gr = count("group"), bo = count("bot");
    const owned = picked.filter((p) => p.owner).length;
    const parts = [];
    if (ch) parts.push(plural(ch, "channel", "channels"));
    if (gr) parts.push(plural(gr, "group", "groups"));
    if (bo) parts.push(plural(bo, "bot", "bots"));
    if (n > on) parts.push(`${n - on} not shown`);
    $("[data-breakdown]", bar).textContent = parts.length ? `(${parts.join(", ")})` : "";

    // Spell out exactly what "Leave" will do before the user confirms.
    const leave = $('[data-bulk="leave"]', bar);
    if (leave) {
      const lines = [];
      if (ch || gr) lines.push(`Leave ${[ch && plural(ch, "channel", "channels"), gr && plural(gr, "group", "groups")].filter(Boolean).join(" and ")}`);
      if (bo) lines.push(`Block and delete ${plural(bo, "bot", "bots")}`);
      if (owned) lines.push(`(${plural(owned, "chat you own", "chats you own")} will be skipped)`);
      leave.setAttribute("hx-confirm", `${lines.join("\n")}\n\nThis can't be undone from here. Continue?`);
    }

    const all = $("[data-select-all]");
    if (all) {
      all.checked = shown.length > 0 && on === shown.length;
      all.indeterminate = on > 0 && on < shown.length;
    }
  }

  // Quick-select: "all", "none", "unread", "kind:channel", "rec:leave".
  // Kind and suggestion picks add to the current selection so they can be combined.
  function quickSelect(spec) {
    const [field, value] = spec.split(":");
    if (spec === "none") selected.clear();
    boxes().forEach((b) => {
      if (spec === "all") b.checked = true;
      else if (spec === "none") b.checked = false;
      else if (spec === "unread") b.checked ||= Number(b.dataset.unread) > 0;
      else if (b.dataset[field] === value) b.checked = true;
      record(b);
    });
    updateBulkBar();
  }

  function setFilter(name, value) {
    const form = $("#filters");
    const field = form && form.elements[name];
    if (!field) return;
    field.value = field.value === value ? "" : value; // click again to clear
    field.dispatchEvent(new Event("change", { bubbles: true }));
  }

  document.addEventListener("change", (e) => {
    if (e.target.matches("[data-select-all]")) {
      boxes().forEach((b) => {
        b.checked = e.target.checked;
        record(b);
      });
    } else if (e.target.matches('#bulk input[name="keys"]')) {
      record(e.target);
    }
    if (e.target.closest("#bulk")) updateBulkBar();
  });

  document.addEventListener("click", (e) => {
    const pick = e.target.closest("[data-select]");
    if (pick) quickSelect(pick.dataset.select);
    const chip = e.target.closest("[data-filter]");
    if (chip) setFilter(chip.dataset.filter, chip.dataset.value);
    if (e.target.closest("[data-clear-selection]")) clearSelection();
    if (e.target.closest("[data-reset]")) {
      const form = $("#filters");
      form.reset();
      $$("input, select", form).forEach((el) => (el.value = el.name === "sort" ? "activity" : ""));
      form.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });

  // Filters changed: restore the selection on the new rows and keep the
  // overview chips' highlight in sync.
  document.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail.target.id === "chats") {
      boxes().forEach((b) => {
        b.checked = selected.has(b.value);
        if (b.checked) record(b); // refresh kind/owner in case the chat changed
      });
      updateBulkBar();
      const form = $("#filters");
      if (form) {
        $$("[data-filter]").forEach((chip) => {
          chip.classList.toggle("active", form.elements[chip.dataset.filter]?.value === chip.dataset.value);
        });
      }
    }
  });

  // Bulk actions apply to the whole selection, including chats the current
  // filters hide, so send every selected key rather than just the visible boxes.
  document.addEventListener("htmx:configRequest", (e) => {
    if (!e.detail.elt.matches?.("[data-bulk]")) return;
    const form = e.detail.formData;
    if (form) {
      form.delete("keys");
      selected.forEach((_, key) => form.append("keys", key));
    } else {
      e.detail.parameters.keys = [...selected.keys()];
    }
  });

  // A bulk job started: clear the selection so it isn't re-submitted by accident.
  document.addEventListener("htmx:afterRequest", (e) => {
    if (e.detail.elt.matches?.("[data-bulk]") && e.detail.successful) clearSelection();
  });

  let toastTimer;
  document.body.addEventListener("toast", (e) => {
    const el = $("#toast");
    el.textContent = e.detail.value;
    el.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.remove("show"), 4000);
  });

  // Surface server errors instead of failing silently.
  document.body.addEventListener("htmx:responseError", (e) => {
    const el = $("#toast");
    el.textContent = e.detail.xhr.responseText || "Request failed";
    el.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.remove("show"), 6000);
  });
})();
