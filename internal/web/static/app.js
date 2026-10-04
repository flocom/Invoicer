// Invoicer — progressive enhancements only. Every page works without JS.
(function () {
  "use strict";

  // ---- confirmations (on forms or on submit buttons) ----
  document.addEventListener("submit", function (e) {
    var form = e.target;
    var btn = e.submitter;
    var msg = (btn && btn.getAttribute("data-confirm")) || form.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
      return;
    }
    // avoid double submissions while keeping the clicked button's value
    if (btn) setTimeout(function () { btn.disabled = true; }, 0);
    setTimeout(function () { if (btn) btn.disabled = false; }, 8000);
  });

  // ---- copy to clipboard ----
  document.addEventListener("click", function (e) {
    var el = e.target.closest("[data-copy]");
    if (!el) return;
    var text = el.getAttribute("data-copy");
    var done = function () {
      el.classList.add("copied");
      var old = el.getAttribute("aria-label");
      el.setAttribute("aria-label", "✓");
      setTimeout(function () { el.classList.remove("copied"); if (old) el.setAttribute("aria-label", old); }, 1500);
    };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done);
    } else {
      var ta = document.createElement("textarea");
      ta.value = text; document.body.appendChild(ta); ta.select();
      try { document.execCommand("copy"); done(); } catch (_) {}
      ta.remove();
    }
  });

  // ---- auto-submit filters ----
  document.querySelectorAll("[data-autosubmit]").forEach(function (el) {
    el.addEventListener("change", function () { el.form.submit(); });
  });

  // ---- browser timezone on first setup ----
  document.querySelectorAll("[data-timezone]").forEach(function (el) {
    try { el.value = Intl.DateTimeFormat().resolvedOptions().timeZone || ""; } catch (_) {}
  });

  // ---- close <details> menus when clicking elsewhere ----
  document.addEventListener("click", function (e) {
    document.querySelectorAll("details.switcher[open], details.menu[open]").forEach(function (d) {
      if (!d.contains(e.target)) d.removeAttribute("open");
    });
  });

  // ---- autocomplete dropdown (addresses, products & services) ----
  var acSeq = 0;
  function autocomplete(field, opts) {
    if (field._ac) return;
    field._ac = true;
    var hadFocus = document.activeElement === field;
    var wrap = document.createElement("div");
    wrap.className = "ac-wrap";
    field.parentNode.insertBefore(wrap, field);
    wrap.appendChild(field);
    if (hadFocus) field.focus(); // moving a node in the DOM drops the focus
    var list = document.createElement("div");
    list.className = "ac-list";
    list.id = "ac-" + (++acSeq);
    list.setAttribute("role", "listbox");
    list.hidden = true;
    wrap.appendChild(list);
    field.setAttribute("aria-autocomplete", "list");
    field.setAttribute("aria-controls", list.id);
    var items = [], active = -1, timer = null, seq = 0;

    function close() { list.hidden = true; active = -1; field.removeAttribute("aria-activedescendant"); }
    function highlight(i) {
      var nodes = list.querySelectorAll(".ac-item");
      nodes.forEach(function (n, j) { n.classList.toggle("active", j === i); });
      active = i;
      if (nodes[i]) { field.setAttribute("aria-activedescendant", nodes[i].id); nodes[i].scrollIntoView({ block: "nearest" }); }
    }
    function show(res) {
      items = res.items || [];
      list.textContent = "";
      if (!items.length) { close(); return; }
      items.forEach(function (it, i) {
        var el = document.createElement("div");
        el.className = "ac-item";
        el.id = list.id + "-" + i;
        el.setAttribute("role", "option");
        var main = document.createElement("span");
        main.textContent = it.label;
        el.appendChild(main);
        if (it.meta) { var m = document.createElement("span"); m.className = "ac-meta"; m.textContent = it.meta; el.appendChild(m); }
        el.addEventListener("mousedown", function (e) { e.preventDefault(); pick(i); });
        list.appendChild(el);
      });
      if (res.footer) { var f = document.createElement("div"); f.className = "ac-foot"; f.textContent = res.footer; list.appendChild(f); }
      list.hidden = false;
      active = -1;
    }
    function pick(i) { var it = items[i]; close(); if (it) opts.pick(it); }
    function query() {
      var q = opts.query();
      if (q === null || q.length < opts.min) { close(); return; }
      var mine = ++seq;
      opts.fetch(q).then(function (res) { if (mine === seq && document.activeElement === field) show(res); }).catch(close);
    }
    field.addEventListener("input", function () { clearTimeout(timer); timer = setTimeout(query, 250); });
    field.addEventListener("keydown", function (e) {
      if (list.hidden) return;
      if (e.key === "ArrowDown") { e.preventDefault(); highlight(Math.min(active + 1, items.length - 1)); }
      else if (e.key === "ArrowUp") { e.preventDefault(); highlight(Math.max(active - 1, 0)); }
      else if (e.key === "Enter" && active >= 0) { e.preventDefault(); pick(active); }
      else if (e.key === "Escape") { close(); }
    });
    field.addEventListener("blur", function () { setTimeout(close, 120); });
  }

  function getJSON(url) {
    return fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } }).then(function (r) {
      if (!r.ok) throw new Error(r.status);
      return r.json();
    });
  }

  function token() {
    var a = new Uint8Array(16);
    crypto.getRandomValues(a);
    return Array.from(a, function (b) { return ("0" + b.toString(16)).slice(-2); }).join("");
  }

  // postal addresses: swisstopo (CH), OpenStreetMap or Google (elsewhere)
  document.querySelectorAll("textarea[data-address]").forEach(function (ta) {
    var session = token();
    autocomplete(ta, {
      min: 3,
      query: function () {
        // only while typing the first line
        var before = ta.value.slice(0, ta.selectionStart || 0);
        if (before.indexOf("\n") !== -1) return null;
        return ta.value.split("\n")[0].trim();
      },
      fetch: function (q) {
        var langSel = ta.form && ta.form.querySelector("select[name=lang]");
        var lang = langSel ? "&lang=" + encodeURIComponent(langSel.value) : "";
        return getJSON("/api/address?q=" + encodeURIComponent(q) + "&s=" + session + lang).then(function (r) {
          return { items: r.suggestions || [], footer: r.attribution };
        });
      },
      pick: function (it) {
        var fill = function (lines) {
          ta.value = lines.join("\n");
          ta.dispatchEvent(new Event("input"));
          session = token(); // a Google session ends with the selection
        };
        if (it.lines && it.lines.length) fill(it.lines);
        else if (it.id) getJSON("/api/address/place?id=" + encodeURIComponent(it.id) + "&s=" + session).then(function (r) { fill(r.lines || [it.label]); });
      },
    });
  });

  // products & services already invoiced
  function wireLineSuggest(ta) {
    var root = ta.closest("[data-lines]");
    if (!root || !root.getAttribute("data-suggest")) return;
    var form = root.closest("form");
    autocomplete(ta, {
      min: 2,
      query: function () { return ta.value.trim(); },
      fetch: function (q) {
        return getJSON(root.getAttribute("data-suggest") + "?q=" + encodeURIComponent(q)).then(function (r) {
          return { items: (r.suggestions || []).map(function (s) {
            return { label: s.description, meta: s.price + " " + s.currency + " · " + s.tax + " %", s: s };
          }) };
        });
      },
      pick: function (it) {
        var row = ta.closest("[data-line]");
        ta.value = it.s.description;
        var cur = form && form.querySelector("[data-currency-select]");
        if (!cur || cur.value === it.s.currency) row.querySelector("[data-price]").value = it.s.price;
        row.querySelector("[data-tax]").value = it.s.tax;
        ta.dispatchEvent(new Event("input"));
        row.querySelector("[data-price]").dispatchEvent(new Event("input"));
      },
    });
  }
  document.addEventListener("focusin", function (e) {
    if (e.target.matches && e.target.matches("textarea[data-line-desc]")) wireLineSuggest(e.target);
  });

  // ---- invoice / recurring editor ----
  function parseNum(v) {
    v = String(v || "").replace(/[\s  ']/g, "");
    var c = v.lastIndexOf(","), d = v.lastIndexOf(".");
    var sep = Math.max(c, d);
    if (sep >= 0 && v.split(v.charAt(sep)).length > 2) sep = -1; // "1,000,000"
    if (sep >= 0) v = v.slice(0, sep).replace(/[.,]/g, "") + "." + v.slice(sep + 1);
    else v = v.replace(/[.,]/g, "");
    var n = parseFloat(v);
    return isNaN(n) ? 0 : n;
  }

  document.querySelectorAll("[data-lines]").forEach(function (root) {
    var body = root.querySelector("[data-lines-body]");
    var tpl = root.querySelector("[data-line-template]");
    var form = root.closest("form");
    var curSel = form && form.querySelector("[data-currency-select]");
    var lang = root.getAttribute("data-lang") || "en";

    function fmt(cents) {
      var cur = (curSel && curSel.value) || root.getAttribute("data-currency") || "EUR";
      try {
        return new Intl.NumberFormat(lang === "fr" ? "fr-FR" : "en-US", { style: "currency", currency: cur }).format(cents / 100);
      } catch (_) { return (cents / 100).toFixed(2) + " " + cur; }
    }

    function autosize(ta) { ta.style.height = "auto"; ta.style.height = (ta.scrollHeight + 2) + "px"; }

    function recalc() {
      var sub = 0, groups = {};
      body.querySelectorAll("[data-line]").forEach(function (row) {
        var q = Math.round(parseNum(row.querySelector("[data-qty]").value) * 1000);
        var p = Math.round(parseNum(row.querySelector("[data-price]").value) * 100);
        var t = Math.round(parseNum(row.querySelector("[data-tax]").value) * 100);
        var amt = Math.round(q * p / 1000);
        sub += amt;
        groups[t] = (groups[t] || 0) + amt;
        row.querySelector("[data-amount]").textContent = fmt(amt);
      });
      var tax = 0;
      Object.keys(groups).forEach(function (bp) { tax += Math.round(groups[bp] * Number(bp) / 10000); });
      root.querySelector("[data-subtotal]").textContent = fmt(sub);
      root.querySelector("[data-taxtotal]").textContent = fmt(tax);
      root.querySelector("[data-total]").textContent = fmt(sub + tax);
    }

    function wire(row) {
      row.querySelectorAll("input").forEach(function (i) { i.addEventListener("input", recalc); });
      var ta = row.querySelector("textarea");
      if (ta) { ta.addEventListener("input", function () { autosize(ta); }); setTimeout(function () { autosize(ta); }, 0); }
    }

    body.querySelectorAll("[data-line]").forEach(wire);

    root.querySelector("[data-add-line]").addEventListener("click", function () {
      var node = tpl.content.firstElementChild.cloneNode(true);
      body.appendChild(node);
      wire(node);
      recalc();
      node.querySelector("textarea").focus();
    });

    body.addEventListener("click", function (e) {
      var btn = e.target.closest("[data-remove-line]");
      if (!btn) return;
      var rows = body.querySelectorAll("[data-line]");
      var row = btn.closest("[data-line]");
      if (rows.length > 1) row.remove();
      else row.querySelectorAll("input,textarea").forEach(function (i) { if (!i.hasAttribute("data-tax")) i.value = i.hasAttribute("data-qty") ? "1" : ""; });
      recalc();
    });

    if (curSel) curSel.addEventListener("change", recalc);
    recalc();
  });

  // client choice pre-fills language and currency
  document.querySelectorAll("[data-client-select]").forEach(function (sel) {
    var form = sel.form;
    sel.addEventListener("change", function () {
      var opt = sel.options[sel.selectedIndex];
      if (!opt) return;
      var l = opt.getAttribute("data-lang"), c = opt.getAttribute("data-currency");
      var ls = form.querySelector("[data-lang-select]"), cs = form.querySelector("[data-currency-select]");
      if (l && ls) ls.value = l;
      if (c && cs) { cs.value = c; cs.dispatchEvent(new Event("change")); }
    });
  });

  // issue date moves the due date by the payment terms
  document.querySelectorAll("[data-issue-date]").forEach(function (issue) {
    var due = issue.form.querySelector("[data-due-date]");
    if (!due) return;
    var terms = parseInt(due.getAttribute("data-terms") || "30", 10);
    issue.addEventListener("change", function () {
      var d = new Date(issue.value + "T00:00:00Z");
      if (isNaN(d)) return;
      d.setUTCDate(d.getUTCDate() + terms);
      due.value = d.toISOString().slice(0, 10);
    });
  });
})();
