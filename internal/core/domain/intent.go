package domain

// An Intent is the durable, human-authored statement of what a Check wants to
// know. It describes the thing, not the way to find the thing.
//
// Intent is deliberately free of selectors, paths, queries and dialects. That
// omission is the point: because intent survives a redesign of the source, a
// proposed Binding can be verified against it.

// IntentKind names the three shapes of intent Agentd supports.
type IntentKind string

const (
	// IntentScalar is a single value: a price, a status, a version string.
	IntentScalar IntentKind = "scalar"
	// IntentRecord is one structured thing with named fields.
	IntentRecord IntentKind = "record"
	// IntentCollection is a repetition of one record shape.
	IntentCollection IntentKind = "collection"
)

// ValueType is the expected type of an extracted value. Agentd keeps this
// small on purpose: it is used to sanity-check an extraction, not to model
// the source's schema.
type ValueType string

const (
	TypeString    ValueType = "string"
	TypeNumber    ValueType = "number"
	TypeBool      ValueType = "bool"
	TypeTimestamp ValueType = "timestamp"
)

// Valid reports whether t is a known value type.
func (t ValueType) Valid() bool {
	switch t {
	case TypeString, TypeNumber, TypeBool, TypeTimestamp:
		return true
	}
	return false
}

// Intent is the common behaviour of the three intent shapes.
type Intent interface {
	// Kind reports which shape this is.
	Kind() IntentKind
	// Name is the operator's short label for what is being watched.
	Name() string
	// Description is the operator's own words for what they want to know.
	// A repair proposal is verified against this, so it is not decoration.
	Description() string
	// Validate reports whether the intent is well formed.
	Validate() error
}

// Field is one named part of a record.
type Field struct {
	// Name identifies the field within its record.
	Name string
	// Description is what this field means, in the operator's words.
	Description string
	// Type is the value type expected.
	Type ValueType
	// Required marks a field whose absence makes the whole record suspect.
	// A missing required field is a structural failure; a missing optional
	// one degrades the run.
	Required bool
}

// Validate reports whether f is well formed.
func (f Field) Validate() error {
	if !nonEmpty(f.Name) {
		return invalidf("field needs a name")
	}
	if !f.Type.Valid() {
		return invalidf("field %q has unknown value type %q", f.Name, f.Type)
	}
	return nil
}

// ScalarIntent watches a single value.
type ScalarIntent struct {
	Label   string
	Purpose string
	Type    ValueType
}

var _ Intent = ScalarIntent{}

func (s ScalarIntent) Kind() IntentKind    { return IntentScalar }
func (s ScalarIntent) Name() string        { return s.Label }
func (s ScalarIntent) Description() string { return s.Purpose }

// Validate reports whether s is well formed.
func (s ScalarIntent) Validate() error {
	if !nonEmpty(s.Label) {
		return invalidf("scalar intent needs a label")
	}
	if !nonEmpty(s.Purpose) {
		return invalidf("scalar intent %q needs a description; a repair cannot be verified against a blank intent", s.Label)
	}
	if !s.Type.Valid() {
		return invalidf("scalar intent %q has unknown value type %q", s.Label, s.Type)
	}
	return nil
}

// RecordIntent watches one structured thing: a set of named fields that
// belong together and are expected to appear together.
type RecordIntent struct {
	Label   string
	Purpose string
	Fields  []Field
}

var _ Intent = RecordIntent{}

func (r RecordIntent) Kind() IntentKind    { return IntentRecord }
func (r RecordIntent) Name() string        { return r.Label }
func (r RecordIntent) Description() string { return r.Purpose }

// RequiredFields returns the fields whose absence is structural.
func (r RecordIntent) RequiredFields() []Field {
	var out []Field
	for _, f := range r.Fields {
		if f.Required {
			out = append(out, f)
		}
	}
	return out
}

// Validate reports whether r is well formed.
func (r RecordIntent) Validate() error {
	if !nonEmpty(r.Label) {
		return invalidf("record intent needs a label")
	}
	if !nonEmpty(r.Purpose) {
		return invalidf("record intent %q needs a description; a repair cannot be verified against a blank intent", r.Label)
	}
	if len(r.Fields) == 0 {
		return invalidf("record intent %q needs at least one field", r.Label)
	}
	seen := make(map[string]bool, len(r.Fields))
	for _, f := range r.Fields {
		if err := f.Validate(); err != nil {
			return err
		}
		if seen[f.Name] {
			return invalidf("record intent %q repeats field %q", r.Label, f.Name)
		}
		seen[f.Name] = true
	}
	return nil
}

// CollectionIntent watches a repetition of one record shape: every row of a
// table, every entry in a feed.
//
// A collection's element is a RecordIntent and nothing else. That restriction
// rules out collections of collections, and with them the temptation to model
// paging or cross-page aggregation here. Agentd observes one fetched source
// version at a time.
type CollectionIntent struct {
	Label   string
	Purpose string
	Element RecordIntent

	// MinItems, when above zero, is the count below which the collection is
	// considered structurally broken rather than merely empty. A table that
	// always has rows and today has none is a shape change; a feed that is
	// legitimately empty is not.
	MinItems int
}

var _ Intent = CollectionIntent{}

func (c CollectionIntent) Kind() IntentKind    { return IntentCollection }
func (c CollectionIntent) Name() string        { return c.Label }
func (c CollectionIntent) Description() string { return c.Purpose }

// Validate reports whether c is well formed.
func (c CollectionIntent) Validate() error {
	if !nonEmpty(c.Label) {
		return invalidf("collection intent needs a label")
	}
	if !nonEmpty(c.Purpose) {
		return invalidf("collection intent %q needs a description; a repair cannot be verified against a blank intent", c.Label)
	}
	if c.MinItems < 0 {
		return invalidf("collection intent %q has a negative minimum item count", c.Label)
	}
	if err := c.Element.Validate(); err != nil {
		return err
	}
	return nil
}
