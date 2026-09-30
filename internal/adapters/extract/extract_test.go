package extract_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/champion19007/agentd/internal/adapters/extract"
	"github.com/champion19007/agentd/internal/core/domain"
)

func loadFixture(t *testing.T, filename string) domain.RawResponse {
	t.Helper()
	path := filepath.Join("testdata", filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read fixture %s: %v", filename, err)
	}
	return domain.RawResponse{
		ContentType: "text/html; charset=utf-8",
		Body:        data,
		Status:      200,
	}
}

// 1. Scalar extraction
func TestFixtureScalarExtraction(t *testing.T) {
	raw := loadFixture(t, "scalar.html")
	e := extract.New()

	b := domain.Binding{
		ID:         "b-scalar",
		IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{
			{Target: "price", Expression: ".pricing-card .price", Dialect: extract.DialectCSS},
		},
	}

	res, err := e.Extract(context.Background(), raw, b)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Kind != domain.IntentScalar {
		t.Errorf("Kind = %v, want scalar", res.Kind)
	}
	if res.Scalar.Missing {
		t.Fatal("scalar value was reported missing")
	}
	if res.Scalar.Text != "$49/month" {
		t.Errorf("Scalar.Text = %q, want %q", res.Scalar.Text, "$49/month")
	}
}

// 2. Record extraction
func TestFixtureRecordExtraction(t *testing.T) {
	raw := loadFixture(t, "record.html")
	e := extract.New()

	b := domain.Binding{
		ID:         "b-record",
		IntentKind: domain.IntentRecord,
		Locators: []domain.Locator{
			{Target: "name", Expression: ".plan-container .name", Dialect: extract.DialectCSS},
			{Target: "price", Expression: ".plan-container .details .price", Dialect: extract.DialectCSS},
			{Target: "seats", Expression: ".plan-container .details .seats", Dialect: extract.DialectCSS},
			{Target: "storage", Expression: ".plan-container .details .storage", Dialect: extract.DialectCSS},
		},
	}

	res, err := e.Extract(context.Background(), raw, b)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Kind != domain.IntentRecord {
		t.Errorf("Kind = %v, want record", res.Kind)
	}

	expected := map[string]string{
		"name":    "Enterprise",
		"price":   "$199",
		"seats":   "25 users",
		"storage": "500GB",
	}

	for k, want := range expected {
		val, ok := res.Record[k]
		if !ok || val.Missing {
			t.Errorf("field %q was missing", k)
			continue
		}
		if val.Text != want {
			t.Errorf("field %q = %q, want %q", k, val.Text, want)
		}
	}
}

// 3. Collection extraction
func TestFixtureCollectionExtraction(t *testing.T) {
	raw := loadFixture(t, "collection.html")
	e := extract.New()

	b := domain.Binding{
		ID:         "b-coll",
		IntentKind: domain.IntentCollection,
		Locators: []domain.Locator{
			{Target: domain.CollectionRoot, Expression: ".product-catalog .product-item", Dialect: extract.DialectCSS},
			{Target: "name", Expression: ".name", Dialect: extract.DialectCSS},
			{Target: "price", Expression: ".price", Dialect: extract.DialectCSS},
			{Target: "stock", Expression: ".stock", Dialect: extract.DialectCSS},
		},
	}

	res, err := e.Extract(context.Background(), raw, b)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Kind != domain.IntentCollection {
		t.Errorf("Kind = %v, want collection", res.Kind)
	}
	if len(res.Collection) != 3 {
		t.Fatalf("Collection len = %d, want 3", len(res.Collection))
	}

	row0 := res.Collection[0]
	if row0["name"].Text != "Standard Widget" || row0["price"].Text != "$19.99" {
		t.Errorf("row 0 mismatch: %+v", row0)
	}
	row2 := res.Collection[2]
	if row2["name"].Text != "Enterprise Widget" || row2["price"].Text != "$99.99" {
		t.Errorf("row 2 mismatch: %+v", row2)
	}
}

// 4. Source redesign
func TestFixtureSourceRedesignBreaksOldBinding(t *testing.T) {
	raw := loadFixture(t, "redesign.html")
	e := extract.New()

	// Old binding looking for .pricing-card .price
	oldBinding := domain.Binding{
		ID:         "b-old",
		IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{
			{Target: "price", Expression: ".pricing-card .price", Dialect: extract.DialectCSS},
		},
	}

	res, err := e.Extract(context.Background(), raw, oldBinding)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !res.Scalar.Missing {
		t.Errorf("old binding should have reported missing on redesigned page, got text %q", res.Scalar.Text)
	}

	// New binding adapted to redesign extracts properly
	newBinding := domain.Binding{
		ID:         "b-new",
		IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{
			{Target: "price", Expression: ".tier-card .cost-box .amount", Dialect: extract.DialectCSS},
		},
	}
	newRes, err := e.Extract(context.Background(), raw, newBinding)
	if err != nil {
		t.Fatalf("Extract with new binding: %v", err)
	}
	if newRes.Scalar.Text != "$249" {
		t.Errorf("new binding price = %q, want $249", newRes.Scalar.Text)
	}
}

// 5. Malformed HTML
func TestFixtureMalformedHTML(t *testing.T) {
	raw := loadFixture(t, "malformed.html")
	e := extract.New()

	b := domain.Binding{
		ID:         "b-malformed",
		IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{
			{Target: "price", Expression: ".price", Dialect: extract.DialectCSS},
		},
	}

	// Malformed HTML must be tolerated without panic or fatal crashes
	res, err := e.Extract(context.Background(), raw, b)
	if err != nil {
		t.Fatalf("Extract on malformed HTML failed: %v", err)
	}
	if res.Scalar.Missing {
		t.Error("expected parser to recover .price from malformed HTML")
	}
	if res.Scalar.Text != "$49 unclosed span" {
		t.Errorf("recovered text = %q", res.Scalar.Text)
	}
}

// 6. Empty result
func TestFixtureEmptyResult(t *testing.T) {
	raw := loadFixture(t, "empty.html")
	e := extract.New()

	b := domain.Binding{
		ID:         "b-empty",
		IntentKind: domain.IntentScalar,
		Locators: []domain.Locator{
			{Target: "price", Expression: ".non-existent-selector", Dialect: extract.DialectCSS},
		},
	}

	res, err := e.Extract(context.Background(), raw, b)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !res.Scalar.Missing {
		t.Errorf("expected Missing=true on empty selector match, got %q", res.Scalar.Text)
	}
}

// 7. Irrelevant presentation changes & Hash gate invariance
func TestFixtureIrrelevantPresentationChangesDoNotProduceFalseChanges(t *testing.T) {
	rawOriginal := loadFixture(t, "record.html")
	rawCosmetic := loadFixture(t, "presentation_changes.html")

	e := extract.New()

	b := domain.Binding{
		ID:         "b-record",
		IntentKind: domain.IntentRecord,
		Locators: []domain.Locator{
			{Target: "name", Expression: ".plan-container .name", Dialect: extract.DialectCSS},
			{Target: "price", Expression: ".plan-container .details .price", Dialect: extract.DialectCSS},
			{Target: "seats", Expression: ".plan-container .details .seats", Dialect: extract.DialectCSS},
			{Target: "storage", Expression: ".plan-container .details .storage", Dialect: extract.DialectCSS},
		},
	}

	extOrig, err := e.Extract(context.Background(), rawOriginal, b)
	if err != nil {
		t.Fatalf("Extract original: %v", err)
	}

	extCosmetic, err := e.Extract(context.Background(), rawCosmetic, b)
	if err != nil {
		t.Fatalf("Extract cosmetic: %v", err)
	}

	// Normalization assertion:
	// Even though presentation_changes.html has:
	// - embedded <style> and <script>
	// - dynamic IDs like id=":r2:"
	// - classes like css-987654
	// - extra whitespace and &nbsp; in "  25&nbsp;users  "
	//
	// The extracted values must be completely identical!
	if !extOrig.Equal(extCosmetic) {
		t.Fatalf("extCosmetic != extOrig!\nOriginal: %+v\nCosmetic: %+v", extOrig, extCosmetic)
	}

	// Verify security: script tags were not included in text extraction
	if extCosmetic.Record["price"].Text != "$199" {
		t.Errorf("price = %q, want $199", extCosmetic.Record["price"].Text)
	}
	if extCosmetic.Record["seats"].Text != "25 users" {
		t.Errorf("seats = %q, want '25 users'", extCosmetic.Record["seats"].Text)
	}
}
