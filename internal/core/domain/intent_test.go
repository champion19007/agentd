package domain_test

import (
	"testing"

	"github.com/champion19007/agentd/internal/core/domain"
)

func TestIntentKinds(t *testing.T) {
	tests := []struct {
		intent domain.Intent
		want   domain.IntentKind
	}{
		{priceIntent(), domain.IntentScalar},
		{planRecord(), domain.IntentRecord},
		{planCollection(), domain.IntentCollection},
	}

	for _, tt := range tests {
		t.Run(string(tt.want), func(t *testing.T) {
			if got := tt.intent.Kind(); got != tt.want {
				t.Errorf("Kind = %q, want %q", got, tt.want)
			}
			if err := tt.intent.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
			if tt.intent.Name() == "" {
				t.Error("an intent needs a name")
			}
			if tt.intent.Description() == "" {
				t.Error("an intent needs a description")
			}
		})
	}
}

// TestIntentRequiresADescription pins something that looks like politeness and
// is not. A repair is verified by checking that a candidate binding reproduces
// what the intent describes; an intent with a blank description gives the
// verifier nothing to work with.
func TestIntentRequiresADescription(t *testing.T) {
	intents := []domain.Intent{
		domain.ScalarIntent{Label: "price", Type: domain.TypeNumber},
		domain.RecordIntent{Label: "plan", Fields: planRecord().Fields},
		domain.CollectionIntent{Label: "plans", Element: planRecord()},
	}

	for _, in := range intents {
		t.Run(string(in.Kind()), func(t *testing.T) {
			if err := in.Validate(); err == nil {
				t.Error("an intent with no description should not validate")
			}
		})
	}
}

func TestScalarIntentValidation(t *testing.T) {
	tests := []struct {
		name   string
		intent domain.ScalarIntent
		ok     bool
	}{
		{"valid", priceIntent(), true},
		{"no label", domain.ScalarIntent{Purpose: "p", Type: domain.TypeNumber}, false},
		{"unknown type", domain.ScalarIntent{Label: "price", Purpose: "p", Type: "money"}, false},
		{"no type", domain.ScalarIntent{Label: "price", Purpose: "p"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.intent.Validate()
			if tt.ok && err != nil {
				t.Errorf("Validate: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("Validate accepted an invalid scalar intent")
			}
		})
	}
}

func TestRecordIntentValidation(t *testing.T) {
	dup := planRecord()
	dup.Fields = append(dup.Fields, domain.Field{
		Name: "price", Description: "again", Type: domain.TypeNumber,
	})

	empty := planRecord()
	empty.Fields = nil

	badField := planRecord()
	badField.Fields = []domain.Field{{Name: "price", Type: "money"}}

	tests := []struct {
		name   string
		intent domain.RecordIntent
		ok     bool
	}{
		{"valid", planRecord(), true},
		{"duplicate field", dup, false},
		{"no fields", empty, false},
		{"field with unknown type", badField, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.intent.Validate()
			if tt.ok && err != nil {
				t.Errorf("Validate: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("Validate accepted an invalid record intent")
			}
		})
	}
}

func TestRecordIntentSeparatesRequiredFromOptional(t *testing.T) {
	r := planRecord()

	required := r.RequiredFields()

	if len(required) != 1 || required[0].Name != "price" {
		t.Fatalf("RequiredFields = %+v, want just price", required)
	}
	// The distinction is what lets a missing field be structural in one case
	// and merely degrading in the other.
	if len(r.Fields) == len(required) {
		t.Error("the fixture should have an optional field too, or this test proves nothing")
	}
}

// TestCollectionIsRepeatedRecords pins the modelling rule: a collection
// repeats one record shape, and nothing else. Because Element is a
// RecordIntent rather than an Intent, a collection of collections cannot be
// expressed, which keeps cross-page aggregation out of the model by
// construction rather than by convention.
func TestCollectionIsRepeatedRecords(t *testing.T) {
	c := planCollection()

	if c.Element.Kind() != domain.IntentRecord {
		t.Errorf("a collection's element is %q, want %q", c.Element.Kind(), domain.IntentRecord)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// An invalid element makes the collection invalid: the element is not a
	// separate thing that can rot on its own.
	broken := planCollection()
	broken.Element.Fields = nil
	if err := broken.Validate(); err == nil {
		t.Error("a collection with an invalid element should not validate")
	}
}

func TestCollectionRejectsANegativeMinimum(t *testing.T) {
	c := planCollection()
	c.MinItems = -1

	if err := c.Validate(); err == nil {
		t.Error("Validate accepted a negative minimum item count")
	}
}

func TestValueTypes(t *testing.T) {
	valid := []domain.ValueType{
		domain.TypeString, domain.TypeNumber, domain.TypeBool, domain.TypeTimestamp,
	}
	for _, v := range valid {
		if !v.Valid() {
			t.Errorf("%q should be a valid value type", v)
		}
	}
	for _, v := range []domain.ValueType{"", "money", "Number"} {
		if v.Valid() {
			t.Errorf("%q should not be a valid value type", v)
		}
	}
}

// TestExtractionSatisfiesIntent covers the judgement that decides whether a
// disappointing extraction is a broken binding or merely a thinner result.
func TestExtractionSatisfiesIntent(t *testing.T) {
	present := domain.Value{Text: "49", Type: domain.TypeNumber}
	missing := domain.Value{Type: domain.TypeNumber, Missing: true}

	tests := []struct {
		name   string
		intent domain.Intent
		got    domain.Extraction
		want   domain.FailureClass // empty means satisfied
	}{
		{
			name:   "scalar found",
			intent: priceIntent(),
			got:    domain.Extraction{Kind: domain.IntentScalar, Scalar: present},
		},
		{
			name:   "scalar missing is structural",
			intent: priceIntent(),
			got:    domain.Extraction{Kind: domain.IntentScalar, Scalar: missing},
			want:   domain.ClassStructural,
		},
		{
			name:   "record complete",
			intent: planRecord(),
			got: domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
				"price": present, "seats": present,
			}},
		},
		{
			name:   "record missing required field is structural",
			intent: planRecord(),
			got: domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
				"price": missing, "seats": present,
			}},
			want: domain.ClassStructural,
		},
		{
			name:   "record missing optional field only degrades",
			intent: planRecord(),
			got: domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
				"price": present, "seats": missing,
			}},
			want: domain.ClassSemantic,
		},
		{
			name:   "collection rows complete",
			intent: planCollection(),
			got: domain.Extraction{Kind: domain.IntentCollection, Collection: []domain.Record{
				{"price": present}, {"price": present},
			}},
		},
		{
			name:   "collection row missing required field is structural",
			intent: planCollection(),
			got: domain.Extraction{Kind: domain.IntentCollection, Collection: []domain.Record{
				{"price": present}, {"price": missing},
			}},
			want: domain.ClassStructural,
		},
		{
			name:   "wrong shape entirely is structural",
			intent: priceIntent(),
			got:    domain.Extraction{Kind: domain.IntentCollection},
			want:   domain.ClassStructural,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.got.Satisfies(tt.intent)

			switch {
			case tt.want == "" && got != nil:
				t.Fatalf("Satisfies returned %+v, want nil", got)
			case tt.want != "" && got == nil:
				t.Fatalf("Satisfies returned nil, want a %q failure", tt.want)
			case tt.want != "" && got.Class != tt.want:
				t.Fatalf("Satisfies returned class %q, want %q", got.Class, tt.want)
			}
		})
	}
}

func TestEmptyCollectionIsStructuralOnlyWhenItShouldNotBe(t *testing.T) {
	// A feed that is legitimately empty is not a broken binding.
	optional := planCollection()
	empty := domain.Extraction{Kind: domain.IntentCollection}
	if got := empty.Satisfies(optional); got != nil {
		t.Errorf("an empty collection with no minimum returned %+v, want nil", got)
	}

	// A table that always has rows and today has none is a shape change.
	required := planCollection()
	required.MinItems = 1
	got := empty.Satisfies(required)
	if got == nil || got.Class != domain.ClassStructural {
		t.Errorf("an empty collection below its minimum returned %+v, want a structural failure", got)
	}
}

func TestExtractionEquality(t *testing.T) {
	a := domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
		"price": {Text: "49", Type: domain.TypeNumber},
	}}
	same := domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
		"price": {Text: "49", Type: domain.TypeNumber},
	}}
	different := domain.Extraction{Kind: domain.IntentRecord, Record: domain.Record{
		"price": {Text: "59", Type: domain.TypeNumber},
	}}

	if !a.Equal(same) {
		t.Error("identical extractions should compare equal; otherwise every run reports a change")
	}
	if a.Equal(different) {
		t.Error("different extractions should not compare equal; otherwise a change goes unreported")
	}
	if a.Equal(domain.Extraction{Kind: domain.IntentScalar}) {
		t.Error("extractions of different kinds should not compare equal")
	}
}
