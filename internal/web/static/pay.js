// Invoicer — card forms of the payment page (Stripe Payment Element, PayPal
// card fields). Card details are typed in Stripe's or PayPal's own frames.
// Until a form is ready, or if it cannot load, the buttons that open Stripe
// Checkout or PayPal's page stay.
(function () {
  "use strict";
  var box = document.querySelector("[data-paybox]");
  if (!box) return;
  var submit = box.querySelector("[data-pay-submit]");
  var errBox = box.querySelector("[data-pay-error]");
  var csrf = box.getAttribute("data-csrf");

  function show() {
    box.hidden = false;
    // the top button now leads to the form at the bottom of the page
    document.querySelectorAll("[data-card-fallback]").forEach(function (b) { b.setAttribute("href", "#pay-card"); });
  }
  function fail(msg) {
    errBox.textContent = msg || "";
    errBox.hidden = !msg;
    busy(false);
  }
  function busy(on) {
    submit.disabled = on;
    submit.classList.toggle("is-busy", on);
  }
  function post(url, body) {
    return fetch(url, {
      method: "POST", credentials: "same-origin",
      headers: { "X-CSRF-Token": csrf, "Content-Type": "application/x-www-form-urlencoded" },
      body: body || ""
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (d) {
        if (!r.ok) throw new Error(d.error || r.statusText);
        return d;
      });
    });
  }

  var appearance = {
    theme: "stripe",
    variables: { colorPrimary: "#4f46e5", colorText: "#111827", colorDanger: "#b91c1c", borderRadius: "9px",
      fontFamily: "Inter, system-ui, sans-serif", spacingUnit: "4px" }
  };

  // ---- Stripe ----
  var key = box.getAttribute("data-stripe-key");
  if (key && window.Stripe) {
    var stripe = window.Stripe(key, { locale: box.getAttribute("data-lang") || "auto" });
    var opts = {
      mode: "payment", amount: parseInt(box.getAttribute("data-amount"), 10), currency: box.getAttribute("data-currency"),
      paymentMethodTypes: ["card"], appearance: appearance
    };
    if (box.getAttribute("data-save")) opts.setupFutureUsage = "off_session";
    var elements = stripe.elements(opts);
    var element = elements.create("payment", { layout: "tabs", wallets: { applePay: "auto", googlePay: "auto" } });
    element.on("ready", show);
    element.on("loaderror", function () { box.hidden = true; }); // wrong key, Stripe unreachable: keep the button to Checkout
    element.mount(box.querySelector("[data-stripe-element]"));
    submit.addEventListener("click", function () {
      fail("");
      busy(true);
      elements.submit().then(function (r) {
        if (r.error) throw r.error;
        return post(box.getAttribute("data-intent"));
      }).then(function (d) {
        return stripe.confirmPayment({
          elements: elements, clientSecret: d.clientSecret,
          confirmParams: { return_url: location.origin + box.getAttribute("data-return") }
        });
      }).then(function (r) {
        if (r && r.error) throw r.error;
      }).catch(function (e) { fail(e && e.message); });
    });
    return;
  }

  // ---- PayPal card fields (when the account offers them) ----
  var order = box.getAttribute("data-paypal-order");
  if (order && window.paypal && window.paypal.CardFields) {
    var fields = window.paypal.CardFields({
      createOrder: function () { return post(order).then(function (d) { return d.id; }); },
      onApprove: function (data) {
        return post(box.getAttribute("data-paypal-capture"), "order=" + encodeURIComponent(data.orderID))
          .then(function (d) { location.href = d.redirect; })
          .catch(function (e) { fail(e.message); });
      },
      onError: function (e) { fail(e && e.message); },
      style: {
        "input": { "font-size": "16px", "font-family": "system-ui, sans-serif", "padding": "12px", "color": "#111827" },
        ".invalid": { "color": "#b91c1c" }
      }
    });
    if (!fields.isEligible()) return; // advanced card payments not enabled: keep the button to PayPal's page
    fields.NameField().render("#pp-card-name");
    fields.NumberField().render("#pp-card-number");
    fields.ExpiryField().render("#pp-card-expiry");
    fields.CVVField().render("#pp-card-cvv");
    show();
    submit.addEventListener("click", function () {
      fail("");
      busy(true);
      fields.submit().catch(function (e) { fail(e && e.message); });
    });
  }
})();
