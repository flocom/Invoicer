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
    var footer = "";
    function show(res) {
      items = res.items || [];
      footer = res.footer || "";
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
        if (opts.remove) {
          var x = document.createElement("button");
          x.type = "button";
          x.className = "ac-remove";
          x.textContent = "×";
          x.title = opts.removeLabel || "Remove";
          x.setAttribute("aria-label", x.title);
          x.addEventListener("mousedown", function (e) {
            e.preventDefault();
            e.stopPropagation();
            opts.remove(it).then(function () {
              show({ items: items.filter(function (o) { return o !== it; }), footer: footer });
            }).catch(function () {});
          });
          el.appendChild(x);
        }
        el.addEventListener("mousedown", function (e) { e.preventDefault(); pick(i); });
        list.appendChild(el);
      });
      if (footer) { var f = document.createElement("div"); f.className = "ac-foot"; f.textContent = footer; list.appendChild(f); }
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
      removeLabel: root.getAttribute("data-suggest-remove"),
      remove: function (it) {
        var body = new FormData();
        body.append("description", it.s.description);
        var csrf = form && form.querySelector("input[name=csrf]");
        return fetch(root.getAttribute("data-suggest") + "/hide", { method: "POST", body: body, credentials: "same-origin",
          headers: { Accept: "application/json", "X-CSRF-Token": csrf ? csrf.value : "" } }).then(function (r) {
          if (!r.ok) throw new Error(r.status);
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

  // ---- searchable client picker with inline creation ----
  function fold(s) { return String(s).normalize("NFD").replace(/[̀-ͯ]/g, "").toLowerCase(); }

  var clientDialog = document.querySelector("[data-client-dialog]");
  var dialogPicker = null;
  function openClientDialog(picker, name) {
    if (!clientDialog || typeof clientDialog.showModal !== "function") return false;
    var f = clientDialog.querySelector("form");
    f.reset();
    f.querySelector("[data-client-error]").hidden = true;
    f.querySelectorAll("button").forEach(function (b) { b.disabled = false; });
    f.elements.name.value = name || "";
    dialogPicker = picker;
    clientDialog.showModal();
    f.elements.name.focus();
    return true;
  }
  if (clientDialog) {
    var cform = clientDialog.querySelector("form");
    clientDialog.querySelector("[data-dialog-close]").addEventListener("click", function () { clientDialog.close(); });
    cform.addEventListener("submit", function (e) {
      e.preventDefault();
      var err = cform.querySelector("[data-client-error]");
      var fail = function (msg) {
        err.textContent = msg || cform.getAttribute("data-error");
        err.hidden = false;
        cform.querySelectorAll("button").forEach(function (b) { b.disabled = false; });
      };
      fetch(cform.action, { method: "POST", body: new FormData(cform), credentials: "same-origin",
        headers: { Accept: "application/json", "X-CSRF-Token": cform.elements.csrf.value } })
        .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
        .then(function (res) {
          if (!res.ok || !res.d.id) { fail(res.d.error); return; }
          clientDialog.close();
          if (dialogPicker) dialogPicker.add(res.d);
        })
        .catch(function () { fail(); });
    });
  }

  var comboSeq = 0;
  document.querySelectorAll("select[data-client-select]").forEach(function (sel) {
    var wrap = document.createElement("div");
    wrap.className = "ac-wrap";
    var input = document.createElement("input");
    input.type = "text";
    input.id = sel.id;
    sel.id = sel.id + "_select";
    input.setAttribute("role", "combobox");
    input.setAttribute("autocomplete", "off");
    input.setAttribute("aria-autocomplete", "list");
    input.setAttribute("aria-expanded", "false");
    input.placeholder = sel.getAttribute("data-search-ph") || "";
    input.required = sel.required;
    sel.required = false;
    sel.hidden = true;
    var list = document.createElement("div");
    list.className = "ac-list";
    list.id = "combo-" + (++comboSeq);
    list.setAttribute("role", "listbox");
    list.hidden = true;
    input.setAttribute("aria-controls", list.id);
    wrap.appendChild(input);
    wrap.appendChild(list);
    sel.parentNode.insertBefore(wrap, sel);
    var chooseMsg = sel.options[0] ? sel.options[0].text : "";
    var items = [], active = -1;

    function current() { var o = sel.options[sel.selectedIndex]; return o && o.value ? o : null; }
    function restore() { var o = current(); input.value = o ? o.text : ""; input.setCustomValidity(""); }
    function close() { list.hidden = true; active = -1; input.setAttribute("aria-expanded", "false"); input.removeAttribute("aria-activedescendant"); }
    function highlight(i) {
      var nodes = list.querySelectorAll(".ac-item");
      nodes.forEach(function (n, j) { n.classList.toggle("active", j === i); });
      active = i;
      if (nodes[i]) { input.setAttribute("aria-activedescendant", nodes[i].id); nodes[i].scrollIntoView({ block: "nearest" }); }
    }
    function open(q) {
      var f = fold(q.trim()), sel0 = current();
      items = [];
      Array.prototype.forEach.call(sel.options, function (o) {
        if (o.value && (!f || fold(o.text).indexOf(f) !== -1)) items.push({ opt: o });
      });
      if (clientDialog) items.push({ create: q.trim() });
      list.textContent = "";
      var start = -1;
      items.forEach(function (it, i) {
        var el = document.createElement("div");
        el.className = "ac-item";
        el.id = list.id + "-" + i;
        el.setAttribute("role", "option");
        if (it.opt) {
          el.textContent = it.opt.text;
          if (it.opt === sel0) { el.classList.add("current"); el.setAttribute("aria-selected", "true"); if (!f) start = i; }
        } else {
          el.classList.add("ac-create");
          el.textContent = "+ " + (it.create ? sel.getAttribute("data-create").replace("%s", it.create) : sel.getAttribute("data-create-empty"));
        }
        el.addEventListener("mousedown", function (e) { e.preventDefault(); pick(i); });
        list.appendChild(el);
      });
      if (f && !items.some(function (it) { return it.opt; })) {
        var none = document.createElement("div");
        none.className = "ac-foot";
        none.textContent = sel.getAttribute("data-no-match");
        list.insertBefore(none, list.firstChild);
      }
      list.hidden = false;
      input.setAttribute("aria-expanded", "true");
      highlight(start >= 0 ? start : (f ? 0 : -1));
    }
    function pick(i) {
      var it = items[i];
      close();
      if (!it) return;
      if (it.opt) {
        sel.value = it.opt.value;
        restore();
        sel.dispatchEvent(new Event("change"));
      } else {
        restore();
        openClientDialog(api, it.create);
      }
    }
    var api = {
      add: function (c) {
        var o = document.createElement("option");
        o.value = c.id;
        o.text = c.name;
        o.setAttribute("data-lang", c.lang || "");
        o.setAttribute("data-currency", c.currency || "");
        var before = null;
        for (var k = 1; k < sel.options.length; k++) {
          if (sel.options[k].text.localeCompare(c.name, undefined, { sensitivity: "base" }) > 0) { before = sel.options[k]; break; }
        }
        sel.insertBefore(o, before);
        sel.value = o.value;
        restore();
        sel.dispatchEvent(new Event("change"));
        input.focus();
      },
    };

    restore();
    input.addEventListener("focus", function () { input.select(); });
    input.addEventListener("click", function () { if (list.hidden) open(""); });
    input.addEventListener("input", function () { input.setCustomValidity(input.value ? chooseMsg : ""); open(input.value); });
    input.addEventListener("keydown", function (e) {
      if (e.key === "ArrowDown") { e.preventDefault(); if (list.hidden) open(""); else highlight(Math.min(active + 1, items.length - 1)); }
      else if (e.key === "ArrowUp") { e.preventDefault(); highlight(Math.max(active - 1, 0)); }
      else if (e.key === "Enter" && !list.hidden) { e.preventDefault(); if (active >= 0) pick(active); }
      else if (e.key === "Escape" && !list.hidden) { e.preventDefault(); close(); restore(); }
    });
    input.addEventListener("blur", function () {
      setTimeout(function () {
        close();
        if (!input.value.trim() && current()) { sel.value = ""; sel.dispatchEvent(new Event("change")); }
        restore();
      }, 120);
    });

    // the plain link stays as a fallback without JavaScript
    var link = sel.closest(".field") && sel.closest(".field").querySelector("[data-client-new]");
    if (link) link.addEventListener("click", function (e) { if (openClientDialog(api, "")) e.preventDefault(); });
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
