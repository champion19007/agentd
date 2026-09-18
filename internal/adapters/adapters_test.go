// Package adapters_test exercises the driven adapters against fakes of the
// outside world: httptest servers, temporary directories and a scripted
// subprocess. Nothing here needs the network, a provider account or a real
// plugin, so CI stays self-contained.
package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/adapters/extract"
	"github.com/champion19007/agentd/internal/adapters/httpsource"
	"github.com/champion19007/agentd/internal/adapters/model"
	"github.com/champion19007/agentd/internal/adapters/notify"
	"github.com/champion19007/agentd/internal/adapters/secrets"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/ports"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// --- secrets ----------------------------------------------------------------

func TestEnvSecretsAreScopedToAPrefix(t *testing.T) {
	t.Setenv("AGENTD_SECRET_API_TOKEN", "s3cret")
	t.Setenv("PATH_LIKE_THING", "should not be reachable")

	bundle, err := secrets.Env{}.Resolve(context.Background(), []domain.SecretRef{"api-token"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, ok := bundle.Get("api-token")
	if !ok {
		t.Fatal("the secret was not resolved")
	}
	if got.Reveal() != "s3cret" {
		t.Errorf("Reveal = %q, want the configured value", got.Reveal())
	}

	// The prefix is what stops a check definition naming an arbitrary
	// environment variable and having Agentd read it out.
	if _, err := (secrets.Env{}).Resolve(context.Background(), []domain.SecretRef{"path-like-thing"}); err == nil {
		t.Error("a variable outside the prefix was readable")
	}
}

func TestMissingSecretIsAuthNotTransient(t *testing.T) {
	_, err := secrets.Env{}.Resolve(context.Background(), []domain.SecretRef{"absent"})
	if err == nil {
		t.Fatal("a missing secret resolved")
	}
	// Retrying will not conjure a credential, so classifying it as transient
	// would make Agentd hammer a source it can never authenticate to.
	if got := domain.Classify(err); got.Class != domain.ClassAuth {
		t.Errorf("Class = %q, want %q", got.Class, domain.ClassAuth)
	}
}

func TestSecretsAreRedactedWhenFormatted(t *testing.T) {
	t.Setenv("AGENTD_SECRET_API_TOKEN", "s3cret")
	bundle, err := secrets.Env{}.Resolve(context.Background(), []domain.SecretRef{"api-token"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// A stray %v anywhere in Agentd must not print a credential.
	for _, formatted := range []string{
		strings.TrimSpace(sprintf("%v", bundle)),
		strings.TrimSpace(sprintf("%s", bundle)),
		strings.TrimSpace(sprintf("%#v", bundle)),
	} {
		if strings.Contains(formatted, "s3cret") {
			t.Fatalf("a secret leaked through formatting: %s", formatted)
		}
	}
}

func sprintf(format string, args ...any) string {
	return strings.TrimSpace(fmt.Sprintf(format, args...))
}

func TestDirSecretsRejectPathEscapes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "api-token"), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := &secrets.Dir{Path: dir}

	bundle, err := resolver.Resolve(context.Background(), []domain.SecretRef{"api-token"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, _ := bundle.Get("api-token"); got.Reveal() != "s3cret" {
		t.Errorf("Reveal = %q, want the file contents with the newline trimmed", got.Reveal())
	}

	// Configuration must not be able to name a file outside the directory.
	for _, bad := range []domain.SecretRef{"../secrets", "..", "sub/dir"} {
		if _, err := resolver.Resolve(context.Background(), []domain.SecretRef{bad}); err == nil {
			t.Errorf("a reference of %q escaped the secrets directory", bad)
		}
	}
}

func TestSecretChainFallsThrough(t *testing.T) {
	t.Setenv("AGENTD_SECRET_FROM_ENV", "env-value")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "from-file"), []byte("file-value"), 0o600); err != nil {
		t.Fatal(err)
	}

	chain := secrets.Chain{&secrets.Dir{Path: dir}, secrets.Env{}}
	bundle, err := chain.Resolve(context.Background(), []domain.SecretRef{"from-file", "from-env"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, _ := bundle.Get("from-file"); got.Reveal() != "file-value" {
		t.Errorf("from-file = %q", got.Reveal())
	}
	if got, _ := bundle.Get("from-env"); got.Reveal() != "env-value" {
		t.Errorf("from-env = %q", got.Reveal())
	}
}

// --- http source ------------------------------------------------------------

func TestHTTPFetchSendsSecretsAsHeaders(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>49</html>"))
	}))
	defer srv.Close()

	src := httpsource.New(httpsource.Options{})
	raw, err := src.Fetch(context.Background(), domain.SourceSpec{
		Kind:          domain.SourceHTTP,
		URL:           srv.URL,
		SecretHeaders: map[string]domain.SecretRef{"Authorization": "api-token"},
	}, domain.SecretBundle{"api-token": domain.NewSecret("Bearer s3cret")})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the resolved secret", gotAuth)
	}
	if gotUA == "" {
		t.Error("Agentd fetched anonymously; being identifiable is the courteous default")
	}
	if string(raw.Body) != "<html>49</html>" {
		t.Errorf("Body = %q", raw.Body)
	}
}

// TestHTTPStatusClassification is where most of this adapter's value lives.
// The class decides whether Agentd retries quietly, wakes somebody, or asks a
// model for a repair.
func TestHTTPStatusClassification(t *testing.T) {
	tests := []struct {
		status int
		want   domain.FailureClass
	}{
		{http.StatusTooManyRequests, domain.ClassRateLimited},
		{http.StatusUnauthorized, domain.ClassAuth},
		{http.StatusForbidden, domain.ClassAuth},
		{http.StatusNotFound, domain.ClassFatal},
		{http.StatusGone, domain.ClassFatal},
		{http.StatusInternalServerError, domain.ClassTransient},
		{http.StatusBadGateway, domain.ClassTransient},
		{http.StatusTeapot, domain.ClassTransient},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			_, err := httpsource.New(httpsource.Options{}).Fetch(context.Background(),
				domain.SourceSpec{Kind: domain.SourceHTTP, URL: srv.URL}, nil)
			if err == nil {
				t.Fatal("a failing status produced no error")
			}

			got := domain.Classify(err)
			if got.Class != tt.want {
				t.Errorf("Class = %q, want %q", got.Class, tt.want)
			}
			// No adapter may report structural. Whether a source changed shape
			// is a judgement about intent, which this layer cannot make.
			if got.Class == domain.ClassStructural {
				t.Error("the source adapter reported a structural failure")
			}
		})
	}
}

func TestHTTPRefusesNonWebSchemes(t *testing.T) {
	// file:// would turn a check definition into arbitrary local file read.
	for _, bad := range []string{"file:///etc/passwd", "ftp://example.test/x", "gopher://x"} {
		_, err := httpsource.New(httpsource.Options{}).Fetch(context.Background(),
			domain.SourceSpec{Kind: domain.SourceHTTP, URL: bad}, nil)
		if err == nil {
			t.Errorf("%q was fetched", bad)
			continue
		}
		if got := domain.Classify(err); got.Class != domain.ClassFatal {
			t.Errorf("%q classified as %q, want fatal", bad, got.Class)
		}
	}
}

func TestHTTPCapsResponseSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()

	_, err := httpsource.New(httpsource.Options{MaxBytes: 128}).Fetch(context.Background(),
		domain.SourceSpec{Kind: domain.SourceHTTP, URL: srv.URL}, nil)
	if err == nil {
		t.Fatal("an oversized response was accepted")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %v, want one that explains the size limit", err)
	}
}

// --- extraction -------------------------------------------------------------

func TestCSSExtraction(t *testing.T) {
	page := `<html><body>
		<div class="plan plan--standard"><span class="price">49</span></div>
		<div class="plan plan--pro"><span class="price">99</span></div>
		<meta name="version" content="2.4.1">
	</body></html>`

	raw := domain.RawResponse{ContentType: "text/html", Body: []byte(page)}
	e := extract.New()

	tests := []struct {
		name string
		expr string
		want string
	}{
		{"descendant", ".plan--standard .price", "49"},
		{"class only takes the first", ".price", "49"},
		{"child combinator", ".plan--pro > .price", "99"},
		{"attribute selector with value", "meta[name=version]@content", "2.4.1"},
		{"tag and class", "span.price", "49"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Extract(context.Background(), raw, binding(domain.IntentScalar,
				domain.Locator{Target: "price", Dialect: extract.DialectCSS, Expression: tt.expr}))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if got.Scalar.Missing {
				t.Fatalf("%q found nothing", tt.expr)
			}
			if got.Scalar.Text != tt.want {
				t.Errorf("Text = %q, want %q", got.Scalar.Text, tt.want)
			}
		})
	}
}

// TestMissingIsNotAnError pins the contract between this adapter and the core.
// Whether a missing value is a broken binding, a thinner result or fine
// depends on which fields the intent marked required, and only the core knows.
func TestMissingIsNotAnError(t *testing.T) {
	raw := domain.RawResponse{ContentType: "text/html", Body: []byte("<html><body></body></html>")}

	got, err := extract.New().Extract(context.Background(), raw, binding(domain.IntentScalar,
		domain.Locator{Target: "price", Dialect: extract.DialectCSS, Expression: ".nowhere"}))
	if err != nil {
		t.Fatalf("a missing value was reported as an error: %v", err)
	}
	if !got.Scalar.Missing {
		t.Error("a value that is not there came back present")
	}
}

func TestCSSCollectionExtraction(t *testing.T) {
	page := `<html><body><table>
		<tr class="plan"><td class="name">Standard</td><td class="price">49</td></tr>
		<tr class="plan"><td class="name">Pro</td><td class="price">99</td></tr>
	</table></body></html>`

	got, err := extract.New().Extract(context.Background(),
		domain.RawResponse{ContentType: "text/html", Body: []byte(page)},
		binding(domain.IntentCollection,
			domain.Locator{Target: domain.CollectionRoot, Dialect: extract.DialectCSS, Expression: "tr.plan"},
			domain.Locator{Target: "name", Dialect: extract.DialectCSS, Expression: ".name"},
			domain.Locator{Target: "price", Dialect: extract.DialectCSS, Expression: ".price"},
		))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(got.Collection) != 2 {
		t.Fatalf("%d rows, want 2", len(got.Collection))
	}
	// Field locators must be resolved inside each row, not across the page.
	if got.Collection[0]["price"].Text != "49" || got.Collection[1]["price"].Text != "99" {
		t.Errorf("rows = %+v, want each row's own price", got.Collection)
	}
	if got.Collection[1]["name"].Text != "Pro" {
		t.Errorf("second row name = %q, want Pro", got.Collection[1]["name"].Text)
	}
}

func TestJSONExtraction(t *testing.T) {
	body := `{"data":{"plans":[{"price":49,"name":"Standard"},{"price":99,"name":"Pro"}],"ok":true}}`
	raw := domain.RawResponse{ContentType: "application/json", Body: []byte(body)}
	e := extract.New()

	tests := []struct{ expr, want string }{
		{"data.plans[0].price", "49"},
		{"data.plans[1].name", "Pro"},
		{"data.ok", "true"},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, err := e.Extract(context.Background(), raw, binding(domain.IntentScalar,
				domain.Locator{Target: "v", Dialect: extract.DialectJSON, Expression: tt.expr}))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if got.Scalar.Text != tt.want {
				t.Errorf("Text = %q, want %q", got.Scalar.Text, tt.want)
			}
		})
	}

	// An object is not a scalar value. Serialising it into something that
	// looks like one would be worse than reporting it missing.
	got, _ := e.Extract(context.Background(), raw, binding(domain.IntentScalar,
		domain.Locator{Target: "v", Dialect: extract.DialectJSON, Expression: "data"}))
	if !got.Scalar.Missing {
		t.Errorf("an object came back as a value: %q", got.Scalar.Text)
	}
}

func TestRegexExtraction(t *testing.T) {
	raw := domain.RawResponse{ContentType: "text/plain", Body: []byte("version: 2.4.1 (build 77)")}

	got, err := extract.New().Extract(context.Background(), raw, binding(domain.IntentScalar,
		domain.Locator{Target: "v", Dialect: extract.DialectRegex, Expression: `version:\s*([0-9.]+)`}))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.Scalar.Text != "2.4.1" {
		t.Errorf("Text = %q, want the first capture group", got.Scalar.Text)
	}
}

// TestFingerprintIgnoresContentButNotShape is what tells a page whose prices
// changed from a page that was redesigned. Getting it backwards would make
// Agentd either miss every break or propose a repair every time a number moved.
func TestFingerprintIgnoresContentButNotShape(t *testing.T) {
	e := extract.New()
	ctx := context.Background()

	original := domain.RawResponse{ContentType: "text/html",
		Body: []byte(`<div class="plan"><span class="price">49</span></div>`)}
	contentChanged := domain.RawResponse{ContentType: "text/html",
		Body: []byte(`<div class="plan"><span class="price">59</span></div>`)}
	redesigned := domain.RawResponse{ContentType: "text/html",
		Body: []byte(`<section data-plan><b data-price>49</b></section>`)}

	a, err := e.Fingerprint(ctx, original)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	b, _ := e.Fingerprint(ctx, contentChanged)
	c, _ := e.Fingerprint(ctx, redesigned)

	if a != b {
		t.Error("a price change moved the fingerprint; every run would look like a redesign")
	}
	if a == c {
		t.Error("a redesign left the fingerprint alone; no break would ever be noticed")
	}
}

func TestJSONFingerprintIgnoresValuesAndLength(t *testing.T) {
	e := extract.New()
	ctx := context.Background()

	one := domain.RawResponse{ContentType: "application/json",
		Body: []byte(`{"plans":[{"price":49,"name":"a"}]}`)}
	many := domain.RawResponse{ContentType: "application/json",
		Body: []byte(`{"plans":[{"price":99,"name":"b"},{"price":1,"name":"c"}]}`)}
	different := domain.RawResponse{ContentType: "application/json",
		Body: []byte(`{"tiers":[{"cost":49}]}`)}

	a, _ := e.Fingerprint(ctx, one)
	b, _ := e.Fingerprint(ctx, many)
	c, _ := e.Fingerprint(ctx, different)

	// A list of one and a list of a thousand identically shaped objects have
	// the same shape.
	if a != b {
		t.Error("adding an item changed the fingerprint")
	}
	if a == c {
		t.Error("renaming every key left the fingerprint alone")
	}
}

func binding(kind domain.IntentKind, locators ...domain.Locator) domain.Binding {
	return domain.Binding{
		ID: "bnd-1", CheckID: "chk-1", DefinitionVersion: 1,
		IntentKind: kind, Fingerprint: "fp-v1", Version: 1,
		Origin: domain.OriginInferred, Locators: locators, DerivedAt: base,
	}
}

// --- model ------------------------------------------------------------------

func TestModelSendsTheKeyAndReadsTheAnswer(t *testing.T) {
	var gotKey string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"price\t.price\nRATIONALE: it moved"}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer srv.Close()

	client := model.New(model.Options{
		Endpoint: srv.URL, APIKeyRef: "model-key",
		Secrets: secrets.Static{"model-key": "sk-test"},
	})

	got, err := client.Complete(context.Background(), ports.ModelRequest{
		Purpose: ports.PurposeRepair, System: "you locate data", Prompt: "find the price",
		Deterministic: true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if gotKey != "sk-test" {
		t.Errorf("x-api-key = %q, want the operator's resolved key", gotKey)
	}
	// A repair proposal must be reproducible: the same break should produce
	// the same proposal so a reviewer sees the same thing twice.
	if temp, ok := gotBody["temperature"]; !ok || temp != 0.0 {
		t.Errorf("temperature = %v, want 0 for a deterministic request", temp)
	}
	if !strings.Contains(got.Text, "RATIONALE") {
		t.Errorf("Text = %q", got.Text)
	}
	if got.Truncated {
		t.Error("a complete answer was reported as truncated")
	}
}

func TestModelReportsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"text","text":"price\t.pri"}],"stop_reason":"max_tokens"}`))
	}))
	defer srv.Close()

	got, err := model.New(model.Options{
		Endpoint: srv.URL, APIKeyRef: "k", Secrets: secrets.Static{"k": "x"},
	}).Complete(context.Background(), ports.ModelRequest{Prompt: "go"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// A half-written locator may well parse and be wrong, so the repair
	// orchestrator discards truncated answers -- which only works if this is
	// reported accurately.
	if !got.Truncated {
		t.Error("a truncated answer was not reported as truncated")
	}
}

func TestModelClassifiesProviderFailures(t *testing.T) {
	tests := []struct {
		status int
		want   domain.FailureClass
	}{
		{http.StatusTooManyRequests, domain.ClassRateLimited},
		{http.StatusUnauthorized, domain.ClassAuth},
		{http.StatusPaymentRequired, domain.ClassAuth},
		{http.StatusInternalServerError, domain.ClassTransient},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(`{"error":{"type":"x","message":"nope"}}`))
			}))
			defer srv.Close()

			_, err := model.New(model.Options{
				Endpoint: srv.URL, APIKeyRef: "k", Secrets: secrets.Static{"k": "x"},
			}).Complete(context.Background(), ports.ModelRequest{Prompt: "go"})
			if err == nil {
				t.Fatal("a failing provider produced no error")
			}
			if got := domain.Classify(err); got.Class != tt.want {
				t.Errorf("Class = %q, want %q", got.Class, tt.want)
			}
		})
	}
}

func TestModelWithoutAKeyFailsAsAuth(t *testing.T) {
	_, err := model.New(model.Options{}).Complete(context.Background(), ports.ModelRequest{Prompt: "go"})
	if err == nil {
		t.Fatal("an unconfigured model client succeeded")
	}
	if got := domain.Classify(err); got.Class != domain.ClassAuth {
		t.Errorf("Class = %q, want %q", got.Class, domain.ClassAuth)
	}
}

// --- notify -----------------------------------------------------------------

func TestWriterNotifierMarksDecisions(t *testing.T) {
	var out strings.Builder
	n := notify.NewWriter(&out)

	if err := n.Deliver(context.Background(), domain.Notification{
		CheckID: "chk-1", Severity: domain.SeverityAlert,
		Subject: "Approve a repair for price?", Body: "it moved",
		NeedsDecision: true, IncidentID: "inc-1", OccurredAt: base,
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	got := out.String()
	// The one message that expects an answer has to say how to give one.
	if !strings.Contains(got, "agentd approve inc-1") {
		t.Errorf("a decision request did not say how to decide:\n%s", got)
	}
	if !strings.Contains(got, "waiting for you") {
		t.Errorf("a decision request did not say it was waiting:\n%s", got)
	}
}

// TestRoutingToNoneIsSilentAndSuccessful: choosing not to be told is a
// legitimate configuration, not a delivery failure.
func TestRoutingToNoneIsSilentAndSuccessful(t *testing.T) {
	var out strings.Builder
	router := notify.NewRouter(notify.NewWriter(&out))

	err := router.Deliver(context.Background(), domain.Notification{
		CheckID: "chk-1", Subject: "something", OccurredAt: base,
		Destination: domain.Destination{Kind: domain.DestinationNone},
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("a check set to tell nobody produced output: %s", out.String())
	}
}

func TestWebhookPostsAStableShape(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	hook := &notify.Webhook{Secrets: secrets.Static{"hook-token": "t0ken"}}
	err := hook.Deliver(context.Background(), domain.Notification{
		CheckID: "chk-1", Severity: domain.SeverityAlert, Subject: "broken",
		Body: "the price moved", NeedsDecision: true, IncidentID: "inc-1",
		OccurredAt:  base,
		Destination: domain.Destination{Kind: domain.DestinationNotify, Target: srv.URL, Secret: "hook-token"},
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if auth != "Bearer t0ken" {
		t.Errorf("Authorization = %q", auth)
	}
	// Anything an operator writes a handler against is an interface Agentd has
	// to keep, so the field names are asserted explicitly.
	for _, key := range []string{"check", "severity", "subject", "body", "needs_decision", "incident", "occurred_at"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the payload is missing %q: %v", key, got)
		}
	}
}

func TestWebhookClassifiesRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := (&notify.Webhook{}).Deliver(context.Background(), domain.Notification{
		CheckID: "chk-1", Subject: "x", OccurredAt: base,
		Destination: domain.Destination{Kind: domain.DestinationNotify, Target: srv.URL},
	})
	if err == nil {
		t.Fatal("a rejected delivery produced no error")
	}
	if got := domain.Classify(err); got.Class != domain.ClassAuth {
		t.Errorf("Class = %q, want %q", got.Class, domain.ClassAuth)
	}
}

func TestNotifierRefusesAnIncompleteNotification(t *testing.T) {
	// A decision request that names no incident cannot be acted on, so it is
	// refused rather than delivered as an unanswerable question.
	err := notify.NewWriter(&strings.Builder{}).Deliver(context.Background(), domain.Notification{
		CheckID: "chk-1", Subject: "decide", NeedsDecision: true, OccurredAt: base,
	})
	if err == nil {
		t.Error("a decision request with no incident was delivered")
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("err = %v, want an invalid-value error", err)
	}
}
