// Package pdf renders invoices as A4 PDF documents in the client's language.
package pdf

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	_ "image/png"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"rsc.io/qr"

	"github.com/flocom/invoicer/internal/brand"
	"github.com/flocom/invoicer/internal/i18n"
	"github.com/flocom/invoicer/internal/money"
	"github.com/flocom/invoicer/internal/store"
)

//go:embed fonts/Lato-Regular.ttf
var fontRegular []byte

//go:embed fonts/Lato-Bold.ttf
var fontBold []byte

type Input struct {
	Invoice *store.Invoice
	Company *store.Company
	Seller  store.Party
	Buyer   store.Party
	PayURL  string // online payment link (empty when Stripe is not set up)
	Today   string
}

const (
	pageW   = 210.0
	margin  = 16.0
	content = pageW - 2*margin
)

type rgb struct{ r, g, b int }

func parseHex(h string) rgb {
	if len(h) != 7 || h[0] != '#' {
		return rgb{67, 56, 202}
	}
	v, err := strconv.ParseUint(h[1:], 16, 32)
	if err != nil {
		return rgb{67, 56, 202}
	}
	return rgb{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
}

func clean(s string) string {
	return strings.NewReplacer(" ", " ", "\r", "").Replace(s)
}

// Render produces the PDF bytes.
func Render(in Input) ([]byte, error) {
	inv, co := in.Invoice, in.Company
	lang := i18n.Norm(inv.Lang)
	t := func(k string, a ...any) string { return i18n.T(lang, k, a...) }
	accent := parseHex(brand.Readable(co.AccentColor)) // white text on it must stay legible
	fmtAmt := func(v int64) string { return clean(money.Format(v, inv.Currency, lang)) }

	p := fpdf.New("P", "mm", "A4", "")
	p.SetMargins(margin, margin, margin)
	p.SetAutoPageBreak(true, 26)
	p.AddUTF8FontFromBytes("lato", "", fontRegular)
	p.AddUTF8FontFromBytes("lato", "B", fontBold)
	title := t("pdf.invoice") + " " + inv.Title()
	p.SetTitle(title, true)
	p.SetAuthor(in.Seller.Name, true)
	p.SetCreator("Invoicer", true)
	p.SetCreationDate(fixedTime(inv))
	p.SetModificationDate(fixedTime(inv))

	footer := i18n.Lines(co.Footer)
	p.SetFooterFunc(func() {
		p.SetY(-22)
		p.SetDrawColor(225, 227, 232)
		p.Line(margin, p.GetY(), pageW-margin, p.GetY())
		p.Ln(2)
		p.SetFont("lato", "", 7.5)
		p.SetTextColor(120, 124, 135)
		for _, l := range footer {
			p.CellFormat(content, 3.6, clean(l), "", 1, "C", false, 0, "")
		}
		p.SetY(-9)
		p.CellFormat(content, 4, fmt.Sprintf("%s · %d/{nb}", inv.Title(), p.PageNo()), "", 0, "R", false, 0, "")
	})
	p.AliasNbPages("{nb}")
	p.AddPage()

	// accent bar
	p.SetFillColor(accent.r, accent.g, accent.b)
	p.Rect(0, 0, pageW, 4, "F")

	// ----- header: logo + title -----
	top := 16.0
	logoH := 0.0
	if len(co.Logo) > 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(co.Logo)); err == nil && cfg.Width > 0 {
			opt := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
			p.RegisterImageOptionsReader("logo", opt, bytes.NewReader(co.Logo))
			w, h := 48.0, 48.0*float64(cfg.Height)/float64(cfg.Width)
			if h > 20 {
				h = 20
				w = h * float64(cfg.Width) / float64(cfg.Height)
			}
			p.ImageOptions("logo", margin, top, w, h, false, opt, 0, "")
			logoH = h
		}
	}
	p.SetXY(pageW-margin-90, top)
	p.SetFont("lato", "B", 22)
	p.SetTextColor(accent.r, accent.g, accent.b)
	p.CellFormat(90, 9, strings.ToUpper(t("pdf.invoice")), "", 2, "R", false, 0, "")
	p.SetFont("lato", "", 10.5)
	p.SetTextColor(40, 44, 55)
	num := inv.Number
	if num == "" {
		num = t("pdf.draft")
	}
	p.CellFormat(90, 5.5, num, "", 2, "R", false, 0, "")

	// ----- seller -----
	y := top + max(logoH, 16) + 6
	p.SetXY(margin, y)
	p.SetTextColor(40, 44, 55)
	p.SetFont("lato", "B", 10.5)
	p.CellFormat(95, 5.5, clean(in.Seller.Name), "", 2, "L", false, 0, "")
	p.SetFont("lato", "", 9)
	p.SetTextColor(90, 95, 108)
	for _, l := range partyLines(in.Seller, t) {
		p.CellFormat(95, 4.4, clean(l), "", 2, "L", false, 0, "")
	}
	sellerEnd := p.GetY()

	// ----- meta box (right) -----
	metaX, metaW := pageW-margin-72, 72.0
	p.SetXY(metaX, y)
	meta := [][2]string{
		{t("pdf.issue_date"), i18n.Date(lang, inv.IssueDate)},
		{t("pdf.due_date"), i18n.Date(lang, inv.DueDate)},
		{t("pdf.currency"), inv.Currency},
	}
	for _, m := range meta {
		p.SetX(metaX)
		p.SetFont("lato", "", 9)
		p.SetTextColor(110, 115, 128)
		p.CellFormat(metaW/2, 5.2, m[0], "", 0, "L", false, 0, "")
		p.SetFont("lato", "B", 9)
		p.SetTextColor(40, 44, 55)
		p.CellFormat(metaW/2, 5.2, clean(m[1]), "", 1, "R", false, 0, "")
	}
	// amount due highlight
	p.SetX(metaX)
	p.SetFillColor(accent.r, accent.g, accent.b)
	p.SetTextColor(255, 255, 255)
	p.SetFont("lato", "", 9)
	p.Ln(1.5)
	p.SetX(metaX)
	p.CellFormat(metaW/2, 8, "  "+t("pdf.amount_due"), "", 0, "L", true, 0, "")
	p.SetFont("lato", "B", 10.5)
	p.CellFormat(metaW/2, 8, fmtAmt(inv.Due())+"  ", "", 1, "R", true, 0, "")
	metaEnd := p.GetY()

	// ----- buyer -----
	y = max(sellerEnd, metaEnd) + 8
	stampY := y + 2
	p.SetXY(margin, y)
	p.SetFont("lato", "B", 8)
	p.SetTextColor(accent.r, accent.g, accent.b)
	p.CellFormat(95, 4.5, strings.ToUpper(t("pdf.bill_to")), "", 2, "L", false, 0, "")
	p.SetTextColor(40, 44, 55)
	p.SetFont("lato", "B", 10.5)
	p.CellFormat(95, 5.5, clean(in.Buyer.Name), "", 2, "L", false, 0, "")
	p.SetFont("lato", "", 9)
	p.SetTextColor(90, 95, 108)
	if in.Buyer.ContactName != "" {
		p.CellFormat(95, 4.4, clean(in.Buyer.ContactName), "", 2, "L", false, 0, "")
	}
	for _, l := range partyLines(store.Party{Address: in.Buyer.Address, Email: in.Buyer.Email, TaxID: in.Buyer.TaxID}, t) {
		p.CellFormat(95, 4.4, clean(l), "", 2, "L", false, 0, "")
	}

	// ----- lines table -----
	p.Ln(7)
	cols := []float64{content - 22 - 30 - 18 - 30, 22, 30, 18, 30}
	heads := []string{t("pdf.description"), t("pdf.qty"), t("pdf.unit_price"), t("pdf.tax"), t("pdf.amount")}
	aligns := []string{"L", "R", "R", "R", "R"}
	drawHead := func() {
		p.SetFillColor(tint(accent, 0.9).r, tint(accent, 0.9).g, tint(accent, 0.9).b)
		p.SetTextColor(accent.r, accent.g, accent.b)
		p.SetFont("lato", "B", 8.5)
		for i, h := range heads {
			txt := h
			if i == 0 {
				txt = "  " + h
			} else if i == len(heads)-1 {
				txt = h + "  "
			}
			p.CellFormat(cols[i], 8, strings.ToUpper(txt), "", 0, aligns[i], true, 0, "")
		}
		p.Ln(-1)
	}
	drawHead()
	p.SetFont("lato", "", 9.2)
	for _, l := range inv.Lines {
		desc := p.SplitText(clean(l.Description), cols[0]-4)
		if len(desc) == 0 {
			desc = []string{""}
		}
		h := float64(len(desc))*4.6 + 3.4
		if p.GetY()+h > 297-30 {
			p.AddPage()
			p.SetY(margin + 4)
			drawHead()
			p.SetFont("lato", "", 9.2)
		}
		x0, y0 := p.GetX(), p.GetY()
		p.SetTextColor(40, 44, 55)
		p.SetXY(x0+2, y0+1.7)
		for _, d := range desc {
			p.SetX(x0 + 2)
			p.CellFormat(cols[0]-4, 4.6, d, "", 2, "L", false, 0, "")
		}
		p.SetXY(x0+cols[0], y0+1.7)
		p.SetTextColor(70, 75, 88)
		p.CellFormat(cols[1], 4.6, clean(money.Quantity(l.Quantity, lang)), "", 0, "R", false, 0, "")
		p.CellFormat(cols[2], 4.6, fmtAmt(l.UnitPrice), "", 0, "R", false, 0, "")
		p.CellFormat(cols[3], 4.6, clean(money.Rate(l.TaxBP, lang))+" %", "", 0, "R", false, 0, "")
		p.SetTextColor(40, 44, 55)
		p.CellFormat(cols[4]-2, 4.6, fmtAmt(l.Amount), "", 0, "R", false, 0, "")
		p.SetXY(x0, y0+h)
		p.SetDrawColor(232, 234, 238)
		p.Line(margin, y0+h, pageW-margin, y0+h)
	}

	// ----- totals -----
	p.Ln(4)
	tw := 78.0
	tx := pageW - margin - tw
	row := func(label, val string, bold bool) {
		if p.GetY() > 297-40 {
			p.AddPage()
			p.SetY(margin + 4)
		}
		p.SetX(tx)
		if bold {
			p.SetFont("lato", "B", 10.5)
			p.SetTextColor(40, 44, 55)
		} else {
			p.SetFont("lato", "", 9.2)
			p.SetTextColor(90, 95, 108)
		}
		p.CellFormat(tw/2+6, 6, label, "", 0, "L", false, 0, "")
		p.CellFormat(tw/2-6, 6, val, "", 1, "R", false, 0, "")
	}
	row(t("pdf.subtotal"), fmtAmt(inv.Subtotal), false)
	for _, g := range store.TaxGroups(inv.Lines) {
		if g.RateBP == 0 && len(store.TaxGroups(inv.Lines)) == 1 {
			row(t("pdf.tax")+" 0 %", fmtAmt(0), false)
			continue
		}
		row(fmt.Sprintf("%s %s %%", t("pdf.tax"), clean(money.Rate(g.RateBP, lang))), fmtAmt(g.Tax), false)
	}
	p.SetDrawColor(210, 213, 220)
	p.Line(tx, p.GetY()+1, pageW-margin, p.GetY()+1)
	p.Ln(2)
	row(t("pdf.total"), fmtAmt(inv.Total), true)
	if inv.AmountPaid > 0 {
		row(t("pdf.paid"), "-"+fmtAmt(inv.AmountPaid), false)
		row(t("pdf.amount_due"), fmtAmt(inv.Due()), true)
	}
	afterTotals := p.GetY()

	// ----- payment info -----
	p.SetY(afterTotals + 6)
	if inv.Status != store.StatusVoid && inv.Due() > 0 && (co.HasBank() || in.PayURL != "") {
		if p.GetY() > 297-70 {
			p.AddPage()
			p.SetY(margin + 4)
		}
		boxY := p.GetY()
		p.SetFont("lato", "B", 8)
		p.SetTextColor(accent.r, accent.g, accent.b)
		p.CellFormat(content, 5, strings.ToUpper(t("pdf.payment")), "", 1, "L", false, 0, "")
		p.SetTextColor(60, 65, 78)
		textW := content
		qrPNG := epcQR(co, inv, in.Seller.Name)
		if qrPNG != nil {
			textW = content - 34
		}
		if co.HasBank() {
			p.SetFont("lato", "B", 9)
			p.CellFormat(textW, 5, t("pdf.bank_transfer"), "", 1, "L", false, 0, "")
			p.SetFont("lato", "", 9)
			kv := [][2]string{
				{t("pdf.account_holder"), firstNonEmpty(co.BankHolder, in.Seller.Name)},
				{t("pdf.bank"), co.BankName},
				{"IBAN", formatIBAN(co.IBAN)},
				{"BIC / SWIFT", co.BIC},
			}
			for _, e := range kv {
				if strings.TrimSpace(e[1]) == "" {
					continue
				}
				p.SetTextColor(110, 115, 128)
				p.CellFormat(32, 4.6, e[0], "", 0, "L", false, 0, "")
				p.SetTextColor(40, 44, 55)
				p.CellFormat(textW-32, 4.6, clean(e[1]), "", 1, "L", false, 0, "")
			}
			for _, l := range i18n.Lines(co.BankExtra) {
				p.SetTextColor(40, 44, 55)
				p.CellFormat(textW, 4.6, clean(l), "", 1, "L", false, 0, "")
			}
			if inv.Number != "" {
				p.SetTextColor(110, 115, 128)
				p.CellFormat(32, 4.6, t("pdf.reference"), "", 0, "L", false, 0, "")
				p.SetTextColor(40, 44, 55)
				p.SetFont("lato", "B", 9)
				p.CellFormat(textW-32, 4.6, inv.Number, "", 1, "L", false, 0, "")
				p.SetFont("lato", "", 9)
			}
		}
		if in.PayURL != "" {
			p.Ln(2)
			p.SetFont("lato", "B", 9)
			p.SetTextColor(40, 44, 55)
			p.CellFormat(textW, 5, t("pdf.pay_online"), "", 1, "L", false, 0, "")
			p.SetFont("lato", "", 9)
			p.SetTextColor(accent.r, accent.g, accent.b)
			p.CellFormat(textW, 4.6, in.PayURL, "", 1, "L", false, 0, in.PayURL)
		}
		if qrPNG != nil {
			opt := fpdf.ImageOptions{ImageType: "PNG"}
			p.RegisterImageOptionsReader("epc", opt, bytes.NewReader(qrPNG))
			p.ImageOptions("epc", pageW-margin-30, boxY+5, 30, 30, false, opt, 0, "")
			p.SetFont("lato", "", 7)
			p.SetTextColor(120, 124, 135)
			p.SetXY(pageW-margin-32, boxY+35.5)
			p.CellFormat(34, 3.5, t("pdf.scan_to_pay"), "", 1, "C", false, 0, "")
			p.SetY(max(p.GetY(), boxY+40))
		}
		p.SetY(max(p.GetY(), boxY) + 4)
	}

	// ----- notes -----
	if strings.TrimSpace(inv.Notes) != "" {
		p.Ln(2)
		p.SetFont("lato", "B", 8)
		p.SetTextColor(accent.r, accent.g, accent.b)
		p.CellFormat(content, 5, strings.ToUpper(t("pdf.notes")), "", 1, "L", false, 0, "")
		p.SetFont("lato", "", 9)
		p.SetTextColor(60, 65, 78)
		p.MultiCell(content, 4.6, clean(inv.Notes), "", "L", false)
	}

	// ----- stamps -----
	stamp := ""
	switch {
	case inv.Status == store.StatusPaid:
		stamp = t("pdf.stamp_paid")
	case inv.Status == store.StatusVoid:
		stamp = t("pdf.stamp_void")
	case inv.Status == store.StatusDraft:
		stamp = t("pdf.stamp_draft")
	}
	if stamp != "" {
		p.SetPage(1)
		p.SetFont("lato", "B", 30)
		c := rgb{22, 163, 74}
		if inv.Status != store.StatusPaid {
			c = rgb{200, 60, 60}
		}
		p.SetTextColor(c.r, c.g, c.b)
		p.SetDrawColor(c.r, c.g, c.b)
		p.SetLineWidth(1)
		p.TransformBegin()
		// right of the "bill to" block, below the meta box: never over the lines
		cx, cy := 150.0, stampY+7
		p.TransformRotate(10, cx, cy)
		p.SetAlpha(0.55, "Normal")
		w := p.GetStringWidth(stamp) + 12
		p.SetXY(cx-w/2, cy-7)
		p.CellFormat(w, 14, stamp, "1", 0, "C", false, 0, "")
		p.SetAlpha(1, "Normal")
		p.TransformEnd()
		p.SetLineWidth(0.2)
	}

	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func partyLines(pt store.Party, t func(string, ...any) string) []string {
	out := i18n.Lines(pt.Address)
	if pt.Email != "" {
		out = append(out, pt.Email)
	}
	if pt.Phone != "" {
		out = append(out, pt.Phone)
	}
	if pt.Website != "" {
		out = append(out, pt.Website)
	}
	if pt.TaxID != "" {
		out = append(out, t("pdf.tax_id")+" "+pt.TaxID)
	}
	if pt.RegistrationID != "" {
		out = append(out, t("pdf.registration")+" "+pt.RegistrationID)
	}
	return out
}

func tint(c rgb, f float64) rgb {
	mix := func(v int) int { return int(float64(v) + (255-float64(v))*f) }
	return rgb{mix(c.r), mix(c.g), mix(c.b)}
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func formatIBAN(s string) string {
	s = strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// epcQR builds a SEPA credit transfer QR code (EPC069-12) for EUR invoices,
// understood by most European banking apps.
func epcQR(co *store.Company, inv *store.Invoice, sellerName string) []byte {
	iban := strings.ToUpper(strings.ReplaceAll(co.IBAN, " ", ""))
	if inv.Currency != "EUR" || iban == "" || inv.Due() <= 0 || inv.Due() > 99999999999 {
		return nil
	}
	name := firstNonEmpty(co.BankHolder, sellerName)
	if len([]rune(name)) > 70 {
		name = string([]rune(name)[:70])
	}
	payload := strings.Join([]string{"BCD", "002", "1", "SCT", strings.ToUpper(strings.ReplaceAll(co.BIC, " ", "")), name, iban,
		"EUR" + money.Input(inv.Due(), 2, true), "", "", inv.Number}, "\n")
	code, err := qr.Encode(payload, qr.M)
	if err != nil {
		return nil
	}
	return code.PNG()
}

// fixedTime keeps the PDF metadata stable for a given invoice revision.
func fixedTime(inv *store.Invoice) time.Time { return time.Unix(inv.UpdatedAt, 0).UTC() }
