// Small client-side helpers: selection count, chip filters, toasts.
(() => {
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

  const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;

  function updateBulkBar() {
    const bar = $("#bulkbar");
    if (!bar) return;
    const boxes = $$('#bulk input[name="keys"]');
    const picked = boxes.filter((b) => b.checked);
    const n = picked.length;
    bar.hidden = n === 0;
    $("[data-count]", bar).textContent = n;

    const count = (kind) => picked.filter((b) => b.dataset.kind === kind).length;
    const ch = count("channel"), gr = count("group"), bo = count("bot");
    const owned = picked.filter((b) => b.dataset.owner === "true").length;
    const parts = [];
    if (ch) parts.push(plural(ch, "channel", "channels"));
    if (gr) parts.push(plural(gr, "group", "groups"));
    if (bo) parts.push(plural(bo, "bot", "bots"));
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
      all.checked = boxes.length > 0 && n === boxes.length;
      all.indeterminate = n > 0 && n < boxes.length;
    }
  }

  // Quick-select: "all", "none", "unread", "kind:channel", "rec:leave".
  // Kind and suggestion picks add to the current selection so they can be combined.
  function quickSelect(spec) {
    const boxes = $$('#bulk input[name="keys"]');
    const [field, value] = spec.split(":");
    boxes.forEach((b) => {
      if (spec === "all") b.checked = true;
      else if (spec === "none") b.checked = false;
      else if (spec === "unread") b.checked ||= Number(b.dataset.unread) > 0;
      else if (b.dataset[field] === value) b.checked = true;
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
      $$('#bulk input[name="keys"]').forEach((b) => (b.checked = e.target.checked));
    }
    if (e.target.closest("#bulk")) updateBulkBar();
  });

  document.addEventListener("click", (e) => {
    const pick = e.target.closest("[data-select]");
    if (pick) quickSelect(pick.dataset.select);
    const chip = e.target.closest("[data-filter]");
    if (chip) setFilter(chip.dataset.filter, chip.dataset.value);
    if (e.target.closest("[data-clear-selection]")) {
      $$('#bulk input[name="keys"]').forEach((b) => (b.checked = false));
      updateBulkBar();
    }
    if (e.target.closest("[data-reset]")) {
      const form = $("#filters");
      form.reset();
      $$("input, select", form).forEach((el) => (el.value = el.name === "sort" ? "activity" : ""));
      form.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });

  // Filters changed: keep the overview chips' highlight in sync.
  document.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail.target.id === "chats") {
      updateBulkBar();
      const form = $("#filters");
      if (form) {
        $$("[data-filter]").forEach((chip) => {
          chip.classList.toggle("active", form.elements[chip.dataset.filter]?.value === chip.dataset.value);
        });
      }
    }
  });

  // A bulk job started: clear the selection so it isn't re-submitted by accident.
  document.addEventListener("htmx:afterRequest", (e) => {
    if (e.detail.elt.matches?.("[data-bulk]") && e.detail.successful) {
      $$('#bulk input[name="keys"]').forEach((b) => (b.checked = false));
      updateBulkBar();
    }
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
