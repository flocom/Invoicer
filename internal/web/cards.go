package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flocom/invoicer/internal/app"
	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/store"
	"github.com/flocom/invoicer/internal/stripe"
)

// clientCards gathers the saved cards section of a client page.
func (s *Server) clientCards(c *Ctx, cl *store.Client) map[string]any {
	d := map[string]any{"HasStripe": c.Company.HasStripe()}
	if !c.Company.HasStripe() {
		return d
	}
	d["Cards"], _ = s.Store.Cards(c.Company.ID, cl.ID)
	if sc, err := s.Store.StripeCustomer(c.Company.ID, cl.ID); err == nil {
		d["CardLink"] = s.App.CardURL(sc)
	}
	open, _, _ := s.Store.Invoices(c.Company.ID, store.InvoiceFilter{ClientID: cl.ID, Status: store.StatusOpen, Limit: 100, Today: s.App.Today()})
	d["Chargeable"] = open
	return d
}

// clientCharge charges an open invoice of the client on the saved card chosen.
func (s *Server) clientCharge(c *Ctx) error {
	cl, err := s.Store.Client(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	back := c.cpath("/clients/%d", cl.ID)
	invID, _ := strconv.ParseInt(c.form("invoice"), 10, 64)
	cardID, _ := strconv.ParseInt(c.form("card"), 10, 64)
	inv, err := s.Store.Invoice(c.Company.ID, invID)
	if err != nil || inv.ClientID != cl.ID {
		c.bad("card.choose_invoice")
		return c.redirect(back)
	}
	card, err := s.Store.Card(c.Company.ID, cl.ID, cardID)
	if err != nil {
		c.bad("card.choose_card")
		return c.redirect(back)
	}
	s.chargeAndReport(c, inv, card)
	return c.redirect(back)
}

// invoiceCharge charges an open invoice on one of its client's saved cards.
func (s *Server) invoiceCharge(c *Ctx) error {
	inv, err := s.loadInvoice(c)
	if err != nil {
		return err
	}
	back := c.cpath("/invoices/%d", inv.ID)
	cardID, _ := strconv.ParseInt(c.form("card"), 10, 64)
	card, err := s.Store.Card(c.Company.ID, inv.ClientID, cardID)
	if err != nil {
		c.bad("card.choose_card")
		return c.redirect(back)
	}
	s.chargeAndReport(c, inv, card)
	return c.redirect(back)
}

// chargeAndReport charges the card now and flashes the outcome.
func (s *Server) chargeAndReport(c *Ctx, inv *store.Invoice, card *store.SavedCard) {
	ctx, cancel := context.WithTimeout(c.R.Context(), 40*time.Second)
	defer cancel()
	ch, err := s.App.ChargeInvoice(ctx, c.Company, inv, card, false, c.User.ID)
	switch {
	case err == nil && ch.Status == "processing":
		c.ok("card.charge_processing", inv.Number)
	case err == nil:
		c.audit("card.charge", inv.Number+" — "+card.Label())
		c.ok("card.charged", inv.Number)
	case errors.Is(err, app.ErrChargeFailed):
		c.flash("err", c.t("card.charge_failed", inv.Number)+" "+strings.TrimPrefix(err.Error(), app.ErrChargeFailed.Error()+": "))
	default:
		slog.Error("card charge", "invoice", inv.ID, "err", err)
		c.flash("err", c.t("card.charge_failed", inv.Number)+" "+err.Error())
	}
}

func (s *Server) clientCardDefault(c *Ctx) error {
	cl, err := s.Store.Client(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	if _, err := s.Store.Card(c.Company.ID, cl.ID, c.id("card")); err != nil {
		return err
	}
	if err := s.Store.SetDefaultCard(c.Company.ID, cl.ID, c.id("card")); err != nil {
		return err
	}
	c.ok("flash.saved")
	return c.redirect(c.cpath("/clients/%d", cl.ID))
}

// clientCardDelete forgets a card here and detaches it on Stripe.
func (s *Server) clientCardDelete(c *Ctx) error {
	cl, err := s.Store.Client(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	card, err := s.Store.Card(c.Company.ID, cl.ID, c.id("card"))
	if err != nil {
		return err
	}
	if c.Company.HasStripe() {
		ctx, cancel := context.WithTimeout(c.R.Context(), 20*time.Second)
		defer cancel()
		if err := stripe.DetachCard(ctx, s.App.StripeKey(c.Company), card.PaymentMethod); err != nil {
			slog.Warn("stripe detach card", "card", card.ID, "err", err)
		}
	}
	if err := s.Store.DeleteCard(c.Company.ID, cl.ID, card.ID); err != nil {
		return err
	}
	c.audit("card.deleted", cl.Name+" — "+card.Label())
	c.ok("card.deleted")
	return c.redirect(c.cpath("/clients/%d", cl.ID))
}

// clientCardLink creates the client's "update my card" link (and e-mails it
// when asked).
func (s *Server) clientCardLink(c *Ctx) error {
	cl, err := s.Store.Client(c.Company.ID, c.id("id"))
	if err != nil {
		return err
	}
	back := c.cpath("/clients/%d", cl.ID) + "#cards"
	ctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	defer cancel()
	if c.form("send") == "1" {
		if err := s.App.SendCardLinkEmail(ctx, c.Company, cl); err != nil {
			c.flash("err", err.Error())
			return c.redirect(back)
		}
		c.audit("card.link_sent", cl.Name)
		c.ok("card.link_sent", cl.Email)
		return c.redirect(back)
	}
	if _, err := s.App.EnsureCustomer(ctx, c.Company, cl); err != nil {
		c.flash("err", err.Error())
		return c.redirect(back)
	}
	c.ok("card.link_ready")
	return c.redirect(back)
}

// ---------- public "update my card" page ----------

func (s *Server) publicCardLoad(c *Ctx) (*store.StripeCustomer, *store.Company, *store.Client, error) {
	if !s.limiter.allow("public:"+c.RL, 60, time.Minute) {
		return nil, nil, nil, store.ErrNotFound
	}
	sc, err := s.Store.StripeCustomerByToken(c.R.PathValue("token"))
	if err != nil {
		return nil, nil, nil, err
	}
	co, err := s.Store.Company(sc.CompanyID)
	if err != nil {
		return nil, nil, nil, err
	}
	cl, err := s.Store.Client(co.ID, sc.ClientID)
	if err != nil {
		return nil, nil, nil, err
	}
	return sc, co, cl, nil
}

func (s *Server) publicCard(c *Ctx) error {
	sc, co, cl, err := s.publicCardLoad(c)
	if err != nil {
		return err
	}
	c.Lang = i18n.Norm(cl.Lang)
	c.User = nil
	c.Company = nil
	saved := false
	if sid := c.R.URL.Query().Get("session_id"); sid != "" && co.HasStripe() {
		// back from Checkout: save the card now rather than waiting for the webhook
		if setup, err := s.Store.CardSetup(sid); err == nil && setup.CompanyID == co.ID && setup.ClientID == cl.ID {
			ctx, cancel := context.WithTimeout(c.R.Context(), 20*time.Second)
			defer cancel()
			if sess, err := stripe.GetCheckout(ctx, s.App.StripeKey(co), sid); err == nil {
				if err := s.App.ApplySetupSession(ctx, co, sess); err != nil {
					slog.Warn("card setup", "err", err)
				}
			}
			if setup, err := s.Store.CardSetup(sid); err == nil && setup.Status == "complete" {
				saved = true
			}
		}
	}
	card, _ := s.Store.DefaultCard(co.ID, cl.ID)
	p := s.page(c, c.t("card.page_title"), "", map[string]any{
		"Co": co, "Client": cl, "Card": card, "Saved": saved, "Token": sc.CardToken, "CanSetup": co.HasStripe(),
		"HasLogo": len(co.Logo) > 0, "SellerName": co.DisplayName(),
	})
	p.Bare = true
	c.W.Header().Set("X-Robots-Tag", "noindex, nofollow")
	c.W.Header().Set("Referrer-Policy", "no-referrer")
	return s.render(c, 200, "card_update", p)
}

func (s *Server) publicCardSetup(c *Ctx) error {
	sc, co, cl, err := s.publicCardLoad(c)
	if err != nil {
		return err
	}
	if !s.limiter.allow("card:"+sc.CardToken, 10, 10*time.Minute) {
		s.renderError(c, http.StatusTooManyRequests, "err.rate_limited")
		return nil
	}
	if !co.HasStripe() {
		http.Redirect(c.W, c.R, "/card/"+sc.CardToken, http.StatusSeeOther)
		return nil
	}
	ctx, cancel := context.WithTimeout(c.R.Context(), 25*time.Second)
	defer cancel()
	url, err := s.App.CardSetupCheckout(ctx, co, sc, i18n.Norm(cl.Lang))
	if err != nil {
		slog.Error("stripe card setup", "client", cl.ID, "err", err)
		c.Lang = i18n.Norm(cl.Lang)
		s.renderError(c, http.StatusBadGateway, "err.checkout")
		return nil
	}
	http.Redirect(c.W, c.R, url, http.StatusSeeOther)
	return nil
}
