// Package extract is a driven adapter implementing ports.Extractor and
// ports.Fingerprinter.
//
// This is the only package that knows what a Locator's Expression means. The
// core carries expressions around and compares them for equality; everything
// about CSS, JSON paths and regular expressions stops here, which is what lets
// a new dialect be added without the domain changing.
//
// A missing value is reported by setting domain.Value.Missing, not by
// returning an error. Whether a missing value is a broken binding, a thinner
// result or perfectly fine depends on which fields the intent marked required,
// and only the core knows that.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

// Dialects this extractor understands.
const (
	// DialectCSS is a subset of CSS selectors over HTML.
	DialectCSS = "css"
	// DialectJSON is a dotted path over a JSON document.
	DialectJSON = "json"
	// DialectRegex is a regular expression whose first capture group is the
	// value. It is the fallback for sources with no structure to speak of.
	DialectRegex = "regex"
)

// Extractor applies bindings.
type Extractor struct{}

var (
	_ ports.Extractor     = Extractor{}
	_ ports.Fingerprinter = Extractor{}
)

// New builds an Extractor.
func New() Extractor { return Extractor{} }

// Dialects reports what this extractor understands.
func (Extractor) Dialects() []string { return []string{DialectCSS, DialectJSON, DialectRegex} }

// Extract applies a binding to a response.
//
// Note what is not set: domain.Value.Type. The extractor is handed a binding,
// not an intent, so it has no idea what type a value was meant to be. Type
// checking belongs to whatever compares the result with the intent, which has
// both.
func (e Extractor) Extract(_ context.Context, raw domain.RawResponse, b domain.Binding) (domain.Extraction, error) {
	doc, err := parse(raw, b)
	if err != nil {
		return domain.Extraction{}, err
	}

	switch b.IntentKind {
	case domain.IntentScalar:
		loc, ok := firstLocator(b)
		if !ok {
			return domain.Extraction{}, fmt.Errorf("extract: binding %q has no locator", b.ID)
		}
		return domain.Extraction{Kind: domain.IntentScalar, Scalar: doc.value(loc)}, nil

	case domain.IntentRecord:
		record := domain.Record{}
		for _, loc := range b.Locators {
			record[loc.Target] = doc.value(loc)
		}
		return domain.Extraction{Kind: domain.IntentRecord, Record: record}, nil

	case domain.IntentCollection:
		root, ok := locatorFor(b, domain.CollectionRoot)
		if !ok {
			return domain.Extraction{}, fmt.Errorf("extract: collection binding %q has no %s locator", b.ID, domain.CollectionRoot)
		}
		var fields []domain.Locator
		for _, loc := range b.Locators {
			if loc.Target != domain.CollectionRoot {
				fields = append(fields, loc)
			}
		}
		rows, err := doc.rows(root, fields)
		if err != nil {
			return domain.Extraction{}, err
		}
		return domain.Extraction{Kind: domain.IntentCollection, Collection: rows}, nil

	default:
		return domain.Extraction{}, fmt.Errorf("extract: binding %q has unknown intent kind %q", b.ID, b.IntentKind)
	}
}

func firstLocator(b domain.Binding) (domain.Locator, bool) {
	for _, l := range b.Locators {
		if l.Target != domain.CollectionRoot {
			return l, true
		}
	}
	return domain.Locator{}, false
}

func locatorFor(b domain.Binding, target string) (domain.Locator, bool) {
	for _, l := range b.Locators {
		if l.Target == target {
			return l, true
		}
	}
	return domain.Locator{}, false
}

// --- documents --------------------------------------------------------------

// document is a parsed source in whichever form its dialect needs.
type document struct {
	dialect string
	html    *html.Node
	json    any
	text    string
}

// parse reads the response once, in the form the binding's dialect needs.
func parse(raw domain.RawResponse, b domain.Binding) (*document, error) {
	dialect := dialectOf(b)
	doc := &document{dialect: dialect, text: string(raw.Body)}

	switch dialect {
	case DialectCSS:
		node, err := html.Parse(strings.NewReader(doc.text))
		if err != nil {
			return nil, fmt.Errorf("extract: the source is not readable as HTML: %w", err)
		}
		doc.html = node
	case DialectJSON:
		if err := json.Unmarshal(raw.Body, &doc.json); err != nil {
			return nil, fmt.Errorf("extract: the source is not readable as JSON: %w", err)
		}
	case DialectRegex:
		// Nothing to parse.
	default:
		return nil, fmt.Errorf("extract: no extractor understands the %q dialect", dialect)
	}
	return doc, nil
}

// dialectOf returns the dialect a binding's locators are written in. They are
// required to agree; a binding mixing dialects would need two extractors and
// the core has no way to express that.
func dialectOf(b domain.Binding) string {
	for _, l := range b.Locators {
		return l.Dialect
	}
	return ""
}

// value applies one locator and returns what it found.
func (d *document) value(loc domain.Locator) domain.Value {
	switch d.dialect {
	case DialectCSS:
		return cssValue(d.html, loc.Expression)
	case DialectJSON:
		return jsonValue(d.json, loc.Expression)
	case DialectRegex:
		return regexValue(d.text, loc.Expression)
	}
	return domain.Value{Missing: true}
}

// rows applies a collection root and then each field locator within every
// element it matched.
func (d *document) rows(root domain.Locator, fields []domain.Locator) ([]domain.Record, error) {
	switch d.dialect {
	case DialectCSS:
		sel, err := parseSelector(root.Expression)
		if err != nil {
			return nil, err
		}
		var out []domain.Record
		for _, node := range sel.matchAll(d.html) {
			record := domain.Record{}
			for _, f := range fields {
				record[f.Target] = cssValue(node, f.Expression)
			}
			out = append(out, record)
		}
		return out, nil

	case DialectJSON:
		items, ok := jsonNode(d.json, root.Expression).([]any)
		if !ok {
			return nil, nil
		}
		var out []domain.Record
		for _, item := range items {
			record := domain.Record{}
			for _, f := range fields {
				record[f.Target] = jsonValue(item, f.Expression)
			}
			out = append(out, record)
		}
		return out, nil

	default:
		return nil, fmt.Errorf("extract: the %q dialect cannot express a repeating element", d.dialect)
	}
}

// --- CSS --------------------------------------------------------------------
//
// A deliberately small subset: tag, #id, .class, [attr], [attr=value], and the
// descendant and child combinators. It covers the selectors a model actually
// proposes for a price on a pricing page, and stops well short of the corners
// of the CSS grammar where a wrong answer would be hard to spot.
//
// An expression may end with @attribute to take an attribute instead of text:
//
//	.plan .price          the text of the first matching element
//	meta[name=version]@content   that element's content attribute

type simpleSelector struct {
	tag     string
	id      string
	classes []string
	attrs   []attrMatch
}

type attrMatch struct {
	name  string
	value string // empty means presence only
	exact bool
}

type selector struct {
	steps []step
}

type step struct {
	sel   simpleSelector
	child bool // true when joined by >
}

// parseSelector reads a selector expression.
func parseSelector(expr string) (*selector, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("extract: empty selector")
	}

	// Normalise "a > b" into tokens so splitting on spaces is enough.
	expr = strings.ReplaceAll(expr, ">", " > ")
	fields := strings.Fields(expr)

	out := &selector{}
	childNext := false
	for _, f := range fields {
		if f == ">" {
			childNext = true
			continue
		}
		s, err := parseSimple(f)
		if err != nil {
			return nil, err
		}
		out.steps = append(out.steps, step{sel: s, child: childNext})
		childNext = false
	}
	if len(out.steps) == 0 {
		return nil, fmt.Errorf("extract: selector %q matched nothing parseable", expr)
	}
	return out, nil
}

func parseSimple(s string) (simpleSelector, error) {
	var out simpleSelector

	// Attribute clauses first, so the remainder is tag/id/class only.
	for {
		open := strings.Index(s, "[")
		if open < 0 {
			break
		}
		close := strings.Index(s[open:], "]")
		if close < 0 {
			return out, fmt.Errorf("extract: selector %q has an unclosed [", s)
		}
		close += open
		clause := s[open+1 : close]
		s = s[:open] + s[close+1:]

		name, value, ok := strings.Cut(clause, "=")
		m := attrMatch{name: strings.TrimSpace(name)}
		if ok {
			m.exact = true
			m.value = strings.Trim(strings.TrimSpace(value), `"'`)
		}
		if m.name == "" {
			return out, fmt.Errorf("extract: selector %q has an empty attribute clause", s)
		}
		out.attrs = append(out.attrs, m)
	}

	for s != "" {
		switch s[0] {
		case '#':
			rest := s[1:]
			cut := strings.IndexAny(rest, ".#")
			if cut < 0 {
				out.id, s = rest, ""
			} else {
				out.id, s = rest[:cut], rest[cut:]
			}
		case '.':
			rest := s[1:]
			cut := strings.IndexAny(rest, ".#")
			if cut < 0 {
				out.classes, s = append(out.classes, rest), ""
			} else {
				out.classes, s = append(out.classes, rest[:cut]), rest[cut:]
			}
		default:
			cut := strings.IndexAny(s, ".#")
			if cut < 0 {
				out.tag, s = strings.ToLower(s), ""
			} else {
				out.tag, s = strings.ToLower(s[:cut]), s[cut:]
			}
		}
	}
	return out, nil
}

// matches reports whether a node satisfies one simple selector.
func (s simpleSelector) matches(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if s.tag != "" && s.tag != "*" && n.Data != s.tag {
		return false
	}
	if s.id != "" && attr(n, "id") != s.id {
		return false
	}
	for _, class := range s.classes {
		if !hasClass(n, class) {
			return false
		}
	}
	for _, a := range s.attrs {
		v, present := attrOK(n, a.name)
		if !present {
			return false
		}
		if a.exact && v != a.value {
			return false
		}
	}
	return true
}

// matchAll returns every node matching the whole selector, in document order.
func (sel *selector) matchAll(root *html.Node) []*html.Node {
	current := []*html.Node{root}

	for _, st := range sel.steps {
		var next []*html.Node
		for _, node := range current {
			if st.child {
				for c := node.FirstChild; c != nil; c = c.NextSibling {
					if st.sel.matches(c) {
						next = append(next, c)
					}
				}
				continue
			}
			walk(node, func(n *html.Node) {
				if n != node && st.sel.matches(n) {
					next = append(next, n)
				}
			})
		}
		current = next
		if len(current) == 0 {
			return nil
		}
	}
	return current
}

func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// cssValue applies a selector and returns the first match's text or attribute.
func cssValue(root *html.Node, expr string) domain.Value {
	path, wanted, hasAttr := strings.Cut(expr, "@")
	sel, err := parseSelector(path)
	if err != nil {
		return domain.Value{Missing: true}
	}

	nodes := sel.matchAll(root)
	if len(nodes) == 0 {
		return domain.Value{Missing: true}
	}

	if hasAttr {
		v, ok := attrOK(nodes[0], wanted)
		if !ok {
			return domain.Value{Missing: true}
		}
		return domain.Value{Text: strings.TrimSpace(v)}
	}
	return domain.Value{Text: textOf(nodes[0])}
}

func textOf(n *html.Node) string {
	var b strings.Builder
	walk(n, func(node *html.Node) {
		if node.Type == html.TextNode {
			// Security & cleanliness: never extract script, style, noscript, or template contents as text
			if node.Parent != nil {
				tag := strings.ToLower(node.Parent.Data)
				if tag == "script" || tag == "style" || tag == "noscript" || tag == "template" {
					return
				}
			}
			b.WriteString(node.Data)
		}
	})
	// Normalize common unicode whitespace (non-breaking spaces, zero-width spaces)
	s := strings.ReplaceAll(b.String(), "\u00a0", " ")
	s = strings.ReplaceAll(s, "\u200b", "")
	return strings.Join(strings.Fields(s), " ")
}

func attr(n *html.Node, name string) string {
	v, _ := attrOK(n, name)
	return v
}

func attrOK(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

func hasClass(n *html.Node, want string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == want {
			return true
		}
	}
	return false
}

// --- JSON -------------------------------------------------------------------
//
// A dotted path with bracket indices: data.plans[0].price. Deliberately not
// JSONPath: the full grammar has filters and wildcards whose failure modes are
// subtle, and a subtle wrong answer is worse here than an obvious missing one.

func jsonValue(doc any, path string) domain.Value {
	node := jsonNode(doc, path)
	if node == nil {
		return domain.Value{Missing: true}
	}
	switch v := node.(type) {
	case string:
		return domain.Value{Text: v}
	case float64:
		return domain.Value{Text: strconv.FormatFloat(v, 'f', -1, 64)}
	case bool:
		return domain.Value{Text: strconv.FormatBool(v)}
	default:
		// Objects and arrays are not scalar values. Reporting them as missing
		// is more honest than serialising them into something that looks like
		// a value and is not.
		return domain.Value{Missing: true}
	}
}

func jsonNode(doc any, path string) any {
	current := doc
	for _, part := range splitPath(path) {
		if current == nil {
			return nil
		}
		if idx, err := strconv.Atoi(part); err == nil {
			arr, ok := current.([]any)
			if !ok || idx < 0 || idx >= len(arr) {
				return nil
			}
			current = arr[idx]
			continue
		}
		obj, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = obj[part]
	}
	return current
}

// splitPath turns "a.b[0].c" into ["a","b","0","c"]. The empty path means the
// document itself.
func splitPath(path string) []string {
	path = strings.TrimSpace(strings.TrimPrefix(path, "$"))
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return nil
	}
	path = strings.NewReplacer("[", ".", "]", "").Replace(path)

	var out []string
	for _, part := range strings.Split(path, ".") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// --- regex ------------------------------------------------------------------

func regexValue(text, expr string) domain.Value {
	re, err := regexp.Compile(expr)
	if err != nil {
		return domain.Value{Missing: true}
	}
	m := re.FindStringSubmatch(text)
	if m == nil {
		return domain.Value{Missing: true}
	}
	// The first capture group when there is one, so that an expression can
	// match context and yield only the interesting part.
	if len(m) > 1 {
		return domain.Value{Text: strings.TrimSpace(m[1])}
	}
	return domain.Value{Text: strings.TrimSpace(m[0])}
}

// --- fingerprinting ---------------------------------------------------------

// Fingerprint summarises a response's shape, ignoring its content.
//
// This is what tells a page whose prices changed from a page that was
// redesigned. It has to work on a source whose binding is already broken --
// that is exactly when it is needed -- so it never consults a binding and
// never fails on unexpected structure.
func (Extractor) Fingerprint(_ context.Context, raw domain.RawResponse) (domain.SourceFingerprint, error) {
	body := strings.TrimSpace(string(raw.Body))
	ctype := strings.ToLower(raw.ContentType)

	switch {
	case strings.Contains(ctype, "json") || strings.HasPrefix(body, "{") || strings.HasPrefix(body, "["):
		var doc any
		if err := json.Unmarshal(raw.Body, &doc); err == nil {
			return fingerprintOf("json:" + jsonSkeleton(doc, 0)), nil
		}
	case strings.Contains(ctype, "html") || strings.Contains(ctype, "xml") || strings.HasPrefix(body, "<"):
		if node, err := html.Parse(strings.NewReader(body)); err == nil {
			return fingerprintOf("html:" + htmlSkeleton(node)), nil
		}
	}

	// Nothing structured to summarise. Length bucket alone is a poor
	// fingerprint but an honest one: it changes when the source changes a lot
	// and not when it changes a little.
	return fingerprintOf(fmt.Sprintf("opaque:%d", len(raw.Body)/512)), nil
}

// htmlSkeleton renders the element structure with ids and classes, and without
// any text. Prices change; the elements around them do not.
func htmlSkeleton(n *html.Node) string {
	var parts []string
	walk(n, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		tag := strings.ToLower(node.Data)
		// Ignore cosmetic/scripting elements that change without affecting content structure
		if tag == "script" || tag == "style" || tag == "noscript" || tag == "template" || tag == "svg" {
			return
		}
		part := tag
		if id := attr(node, "id"); id != "" {
			if !isDynamicID(id) {
				part += "#" + id
			}
		}
		if classes := strings.Fields(attr(node, "class")); len(classes) > 0 {
			var stable []string
			for _, c := range classes {
				if !isDynamicClass(c) {
					stable = append(stable, c)
				}
			}
			if len(stable) > 0 {
				sort.Strings(stable)
				part += "." + strings.Join(stable, ".")
			}
		}
		parts = append(parts, part)
	})
	return strings.Join(parts, ">")
}

// isDynamicID identifies random or generated IDs (e.g. React 18 useId ":r0:", uuid-like, or random hex/numbers).
func isDynamicID(id string) bool {
	if strings.HasPrefix(id, ":r") && strings.HasSuffix(id, ":") {
		return true
	}
	if strings.HasPrefix(id, "ember") || strings.HasPrefix(id, "react-") {
		return true
	}
	if len(id) >= 12 {
		isHex := true
		for _, r := range id {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-') {
				isHex = false
				break
			}
		}
		if isHex {
			return true
		}
	}
	return false
}

// isDynamicClass filters out CSS-in-JS hashes like css-1x2y3z or sc-123abc
func isDynamicClass(c string) bool {
	if strings.HasPrefix(c, "css-") && len(c) > 6 {
		return true
	}
	if strings.HasPrefix(c, "sc-") && len(c) > 5 {
		return true
	}
	return false
}

// jsonSkeleton renders the key structure without values. Array elements are
// summarised by their first member: a list of a thousand identically shaped
// objects has the same shape as a list of one.
func jsonSkeleton(doc any, depth int) string {
	if depth > 12 {
		return "..."
	}
	switch v := doc.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ":" + jsonSkeleton(v[k], depth+1)
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		if len(v) == 0 {
			return "[]"
		}
		return "[" + jsonSkeleton(v[0], depth+1) + "]"
	case string:
		return "s"
	case float64:
		return "n"
	case bool:
		return "b"
	default:
		return "z"
	}
}

func fingerprintOf(s string) domain.SourceFingerprint {
	return domain.SourceFingerprint(domain.Digest([]byte(s)))
}
