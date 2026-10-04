package geo

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestFormatting(t *testing.T) {
	if streetLine("FR", "Rue de la Paix", "12") != "12 Rue de la Paix" || streetLine("DE", "Hauptstraße", "5") != "Hauptstraße 5" {
		t.Fatal("street order")
	}
	if placeLine("US", "94105", "San Francisco", "California") != "San Francisco, California 94105" || placeLine("CH", "8001", "Zürich", "") != "8001 Zürich" {
		t.Fatal("place line")
	}
}

// GEO_LIVE=1 go test ./internal/geo -run Live -v
func TestLiveSearch(t *testing.T) {
	if os.Getenv("GEO_LIVE") == "" {
		t.Skip("set GEO_LIVE=1 to query swisstopo and OpenStreetMap")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, q := range []string{"Bahnhofstrasse 10 Zürich", "Rue du Rhône 1 Genève", "221B Baker Street London", "12 rue de la Paix Paris"} {
		res, err := Search(ctx, Config{Swisstopo: true, Provider: ProviderOSM}, q, "fr", "")
		if err != nil || len(res) == 0 {
			t.Fatalf("%s: %v %v", q, res, err)
		}
		t.Logf("%-28s → [%s] %q", q, res[0].Source, res[0].Lines)
	}
}
