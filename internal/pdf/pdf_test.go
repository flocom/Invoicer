package pdf

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/flocom/invoicer/internal/store"
)

func sample(lang, status string) Input {
	lines := []store.Line{
		{Description: "Développement d'une application web\nSprint 1 — authentification, tableau de bord", Quantity: 2500, UnitPrice: 40000, TaxBP: 2000},
		{Description: "Hébergement mensuel", Quantity: 1000, UnitPrice: 1999, TaxBP: 550},
	}
	sub, tax, total := store.ComputeTotals(lines)
	inv := &store.Invoice{ID: 1, Number: "AC-2026-0001", Status: status, Currency: "EUR", Lang: lang, IssueDate: "2026-10-04",
		DueDate: "2026-11-03", Subtotal: sub, TaxTotal: tax, Total: total, AmountPaid: 20000, Lines: lines,
		Notes: "Merci pour votre confiance.", UpdatedAt: 1791115000}
	co := &store.Company{Name: "Acme Studio", AccentColor: "#4338ca", Footer: "Acme Studio SAS — capital 10 000 € — RCS Paris 123 456 789\nIndemnité forfaitaire pour frais de recouvrement : 40 €"}
	return Input{Invoice: inv, Company: co, PayURL: "https://invoices.example.com/pay/abc",
		Bank:   &store.BankAccount{Currency: "EUR", IBAN: "FR7630006000011234567890189", BIC: "AGRIFRPP", BankName: "Crédit Agricole"},
		Seller: store.Party{Name: "Acme Studio SAS", Address: "12 rue de la Paix\n75002 Paris", Email: "billing@acme.test", TaxID: "FR12345678901", RegistrationID: "123 456 789 00012"},
		Buyer:  store.Party{Name: "Client SARL", ContactName: "Marie Dupont", Address: "1 place Bellecour\n69002 Lyon", TaxID: "FR98765432109"}}
}

func TestRender(t *testing.T) {
	for _, lang := range []string{"en", "fr"} {
		for _, st := range []string{"draft", "open", "paid"} {
			b, err := Render(sample(lang, st))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(b), "%PDF") || len(b) < 5000 {
				t.Fatalf("bad pdf %s %s", lang, st)
			}
			if dir := os.Getenv("PDF_OUT"); dir != "" {
				os.WriteFile(dir+"/sample-"+lang+"-"+st+".pdf", b, 0o644)
			}
		}
	}
}

func TestPaymentQR(t *testing.T) {
	in := sample("fr", "open")
	if code, key := paymentQR(in); code == nil || key != "pdf.scan_to_pay_online" {
		t.Fatalf("with a pay link: %v %s", code != nil, key)
	}
	if !bytes.Equal(must(paymentQR(in)), urlQR(in.PayURL)) {
		t.Fatal("QR should encode the pay link")
	}
	in.PayURL = "" // card payment not offered: SEPA transfer
	if code, key := paymentQR(in); code == nil || key != "pdf.scan_to_pay" {
		t.Fatalf("without a pay link: %v %s", code != nil, key)
	}
	in.Bank = nil
	if code, _ := paymentQR(in); code != nil {
		t.Fatal("no QR without pay link nor bank")
	}
}

func must(b []byte, _ string) []byte { return b }
