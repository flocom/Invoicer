package pdf

import (
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
	co := &store.Company{Name: "Acme Studio", AccentColor: "#4338ca", IBAN: "FR7630006000011234567890189", BIC: "AGRIFRPP",
		BankName: "Crédit Agricole", Footer: "Acme Studio SAS — capital 10 000 € — RCS Paris 123 456 789\nIndemnité forfaitaire pour frais de recouvrement : 40 €"}
	return Input{Invoice: inv, Company: co, PayURL: "https://invoices.example.com/pay/abc",
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
