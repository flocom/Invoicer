package geo

// countryName returns the country name in the document language; unknown
// codes fall back to the provider's own name.
func countryName(cc, lang string) string {
	n, ok := countries[cc]
	if !ok {
		return ""
	}
	if lang == "fr" {
		return n[1]
	}
	return n[0]
}

var countries = map[string][2]string{
	"AD": {"Andorra", "Andorre"}, "AE": {"United Arab Emirates", "Émirats arabes unis"}, "AR": {"Argentina", "Argentine"},
	"AT": {"Austria", "Autriche"}, "AU": {"Australia", "Australie"}, "BE": {"Belgium", "Belgique"}, "BG": {"Bulgaria", "Bulgarie"},
	"BR": {"Brazil", "Brésil"}, "CA": {"Canada", "Canada"}, "CH": {"Switzerland", "Suisse"}, "CL": {"Chile", "Chili"},
	"CN": {"China", "Chine"}, "CO": {"Colombia", "Colombie"}, "CY": {"Cyprus", "Chypre"}, "CZ": {"Czechia", "Tchéquie"},
	"DE": {"Germany", "Allemagne"}, "DK": {"Denmark", "Danemark"}, "DZ": {"Algeria", "Algérie"}, "EE": {"Estonia", "Estonie"},
	"EG": {"Egypt", "Égypte"}, "ES": {"Spain", "Espagne"}, "FI": {"Finland", "Finlande"}, "FR": {"France", "France"},
	"GB": {"United Kingdom", "Royaume-Uni"}, "GR": {"Greece", "Grèce"}, "HK": {"Hong Kong", "Hong Kong"}, "HR": {"Croatia", "Croatie"},
	"HU": {"Hungary", "Hongrie"}, "IE": {"Ireland", "Irlande"}, "IL": {"Israel", "Israël"}, "IN": {"India", "Inde"},
	"IS": {"Iceland", "Islande"}, "IT": {"Italy", "Italie"}, "JP": {"Japan", "Japon"}, "KR": {"South Korea", "Corée du Sud"},
	"LI": {"Liechtenstein", "Liechtenstein"}, "LT": {"Lithuania", "Lituanie"}, "LU": {"Luxembourg", "Luxembourg"},
	"LV": {"Latvia", "Lettonie"}, "MA": {"Morocco", "Maroc"}, "MC": {"Monaco", "Monaco"}, "MT": {"Malta", "Malte"},
	"MU": {"Mauritius", "Maurice"}, "MX": {"Mexico", "Mexique"}, "NL": {"Netherlands", "Pays-Bas"}, "NO": {"Norway", "Norvège"},
	"NZ": {"New Zealand", "Nouvelle-Zélande"}, "PL": {"Poland", "Pologne"}, "PT": {"Portugal", "Portugal"}, "RO": {"Romania", "Roumanie"},
	"RS": {"Serbia", "Serbie"}, "SA": {"Saudi Arabia", "Arabie saoudite"}, "SE": {"Sweden", "Suède"}, "SG": {"Singapore", "Singapour"},
	"SI": {"Slovenia", "Slovénie"}, "SK": {"Slovakia", "Slovaquie"}, "SN": {"Senegal", "Sénégal"}, "TN": {"Tunisia", "Tunisie"},
	"TR": {"Türkiye", "Turquie"}, "UA": {"Ukraine", "Ukraine"}, "US": {"United States", "États-Unis"}, "ZA": {"South Africa", "Afrique du Sud"},
	"CI": {"Côte d'Ivoire", "Côte d'Ivoire"}, "CM": {"Cameroon", "Cameroun"}, "QA": {"Qatar", "Qatar"}, "TH": {"Thailand", "Thaïlande"},
}
