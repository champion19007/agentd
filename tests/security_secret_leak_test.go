package tests

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/champion19007/agentd/internal/adapters/clock"
	"github.com/champion19007/agentd/internal/adapters/extract"
	"github.com/champion19007/agentd/internal/adapters/httpsource"
	"github.com/champion19007/agentd/internal/adapters/model"
	"github.com/champion19007/agentd/internal/adapters/notify"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/core/run"
	"github.com/champion19007/agentd/internal/ports"
	"github.com/champion19007/agentd/internal/store/sqlite"
	_ "modernc.org/sqlite"
)

const canarySecret = "CANARY_SECRET_sk_live_9876543210_SUPERSECRET"

type canarySecretResolver struct {
	secret string
}

func (r *canarySecretResolver) Resolve(ctx context.Context, refs []domain.SecretRef) (domain.SecretBundle, error) {
	bundle := make(domain.SecretBundle)
	for _, ref := range refs {
		bundle[ref] = domain.NewSecret(r.secret)
	}
	return bundle, nil
}

type recordingModel struct {
	capturedPrompts []string
	capturedSystems []string
}

func (m *recordingModel) Complete(ctx context.Context, req ports.ModelRequest) (ports.ModelResponse, error) {
	m.capturedPrompts = append(m.capturedPrompts, req.Prompt)
	m.capturedSystems = append(m.capturedSystems, req.System)
	return ports.ModelResponse{
		Text: `{"verdict": "unchanged", "explanation": "no change detected"}`,
	}, nil
}

type testIDs struct {
	counter int64
}

func (t *testIDs) NewRunID() domain.RunID {
	n := atomic.AddInt64(&t.counter, 1)
	return domain.RunID(fmt.Sprintf("run-canary-%d", n))
}

func (t *testIDs) NewIncidentID() domain.IncidentID {
	n := atomic.AddInt64(&t.counter, 1)
	return domain.IncidentID(fmt.Sprintf("inc-canary-%d", n))
}

func (t *testIDs) NewBindingID() domain.BindingID {
	n := atomic.AddInt64(&t.counter, 1)
	return domain.BindingID(fmt.Sprintf("bin-canary-%d", n))
}

func TestSecurity_SecretsNeverLeak(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentd_secret_test.db")

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer st.Close()

	// 1. Target server that receives the secret in the Authorization header
	var receivedAuthHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body><div id="price">$99.99</div></body></html>`))
	}))
	defer ts.Close()

	recModel := &recordingModel{}
	var notificationBuf bytes.Buffer
	writerNotifier := notify.NewWriter(&notificationBuf)

	ext := extract.New()
	deps := run.Deps{
		Store:       st,
		Clock:       clock.System{},
		IDs:         &testIDs{},
		Source:      httpsource.New(httpsource.Options{AllowPrivateIPs: true}),
		Extract:     ext,
		Fingerprint: ext,
		Model:       recModel,
		Secrets:     &canarySecretResolver{secret: canarySecret},
		Notifier:    writerNotifier,
	}

	checkID := domain.CheckID("chk-canary-secrets")
	refName := domain.SecretRef("auth_bearer_token")

	chkDef := domain.Definition{
		Version: 1,
		Intent: domain.ScalarIntent{
			Label:   "price",
			Purpose: "the price of the item",
			Type:    domain.TypeString,
		},
		Source: domain.SourceSpec{
			Kind: domain.SourceHTTP,
			URL:  ts.URL,
			SecretHeaders: map[string]domain.SecretRef{
				"Authorization": refName,
			},
		},
		Schedule: domain.Schedule{
			Interval: 10 * time.Minute,
		},
		Destination: domain.Destination{
			Kind:   domain.DestinationNotify,
			Target: "console",
		},
		CreatedAt: time.Now().UTC(),
	}

	chk, err := domain.NewCheck(checkID, chkDef)
	if err != nil {
		t.Fatalf("failed to create check: %v", err)
	}

	// Save check and binding
	if err := st.Update(ctx, func(ctx context.Context, tx ports.Tx) error {
		if err := tx.SaveCheck(ctx, chk); err != nil {
			return err
		}
		binding := domain.Binding{
			ID:                "bin-canary-1",
			CheckID:           checkID,
			DefinitionVersion: 1,
			IntentKind:        domain.IntentScalar,
			Fingerprint:       "fp-test",
			Version:           1,
			Origin:            domain.OriginInferred,
			Locators: []domain.Locator{
				{Target: "price", Dialect: "css", Expression: "#price"},
			},
			DerivedAt: time.Now().UTC(),
		}
		if err := tx.SaveBinding(ctx, binding); err != nil {
			return err
		}
		return tx.ActivateBinding(ctx, checkID, 1)
	}); err != nil {
		t.Fatalf("failed to seed check: %v", err)
	}

	// Execute Run 1 (establishes baseline snapshot)
	pipeline := run.New(deps)
	slot1 := domain.Slot(time.Now().UTC().Unix())
	res1, err := pipeline.Run(ctx, chk, slot1)
	if err != nil {
		t.Fatalf("run 1 failed: %v", err)
	}
	if res1.Run.State() != domain.StateQuiet && res1.Run.State() != domain.StateChanged {
		t.Fatalf("expected run 1 success (quiet or changed), got %s", res1.Run.State())
	}

	// Verify the HTTP server actually received the canary secret
	if !strings.Contains(receivedAuthHeader, canarySecret) {
		t.Fatalf("server should have received canary secret, got %q", receivedAuthHeader)
	}

	// Execute Run 2 (triggers comparison & model evaluation if changed)
	slot2 := slot1 + 1
	_, err = pipeline.Run(ctx, chk, slot2)
	if err != nil {
		t.Fatalf("run 2 failed: %v", err)
	}

	// Deliver test notification
	_ = writerNotifier.Deliver(ctx, domain.Notification{
		CheckID:    checkID,
		TraceID:    "tr-canary-123",
		Severity:   domain.SeverityInfo,
		Subject:    "Check price updated",
		Body:       "Price is currently $99.99",
		OccurredAt: time.Now().UTC(),
	})

	// VERIFICATION 1: Prompts rendered to the model must NEVER contain the secret
	for _, p := range recModel.capturedPrompts {
		if strings.Contains(p, canarySecret) {
			t.Fatalf("SECRET LEAK! Canary secret leaked into model prompt:\n%s", p)
		}
	}
	for _, s := range recModel.capturedSystems {
		if strings.Contains(s, canarySecret) {
			t.Fatalf("SECRET LEAK! Canary secret leaked into model system instruction:\n%s", s)
		}
	}

	// VERIFICATION 2: Rendered notifications must NEVER contain the secret
	notifOutput := notificationBuf.String()
	if strings.Contains(notifOutput, canarySecret) {
		t.Fatalf("SECRET LEAK! Canary secret leaked into notifications:\n%s", notifOutput)
	}

	// VERIFICATION 3: Snapshots stored in database must NEVER contain the secret
	snaps, err := st.Snapshots(ctx, checkID)
	if err != nil {
		t.Fatalf("failed to retrieve snapshots: %v", err)
	}
	for _, s := range snaps.All() {
		raw := string(s.Body())
		if strings.Contains(raw, canarySecret) {
			t.Fatalf("SECRET LEAK! Canary secret leaked into snapshot body:\n%s", raw)
		}
	}

	// VERIFICATION 4: Raw SQLite database must NEVER store the raw secret
	// Scan all rows of all tables in the SQLite database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("direct sqlite open failed: %v", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatalf("querying tables failed: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			tables = append(tables, name)
		}
	}
	_ = rows.Close()

	for _, tbl := range tables {
		colRows, err := db.Query("PRAGMA table_info(" + tbl + ")")
		if err != nil {
			continue
		}
		var cols []string
		for colRows.Next() {
			var cid int
			var cname, ctype string
			var notnull, pk int
			var dflt any
			if err := colRows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err == nil {
				cols = append(cols, cname)
			}
		}
		_ = colRows.Close()

		if len(cols) == 0 {
			continue
		}

		dataRows, err := db.Query("SELECT " + strings.Join(cols, ", ") + " FROM " + tbl)
		if err != nil {
			continue
		}
		for dataRows.Next() {
			vals := make([]any, len(cols))
			valPtrs := make([]any, len(cols))
			for i := range vals {
				valPtrs[i] = &vals[i]
			}
			if err := dataRows.Scan(valPtrs...); err == nil {
				for i, v := range vals {
					if strVal, ok := v.(string); ok {
						if strings.Contains(strVal, canarySecret) {
							t.Fatalf("SECRET LEAK! Canary secret found in table %s, col %s: %s", tbl, cols[i], strVal)
						}
					}
					if byteVal, ok := v.([]byte); ok {
						if bytes.Contains(byteVal, []byte(canarySecret)) {
							t.Fatalf("SECRET LEAK! Canary secret found in blob table %s, col %s", tbl, cols[i])
						}
					}
				}
			}
		}
		_ = dataRows.Close()
	}

	// VERIFICATION 5: Scrubbing in model errors
	fakeErrBody := []byte(`{"error": {"type": "invalid_request", "message": "bad key ` + canarySecret + `"}}`)
	resF := model.Classify(401, fakeErrBody, canarySecret)
	if resF != nil && strings.Contains(resF.Detail, canarySecret) {
		t.Fatalf("SECRET LEAK! Canary secret leaked through model error detail: %s", resF.Detail)
	}
	if resF != nil && strings.Contains(resF.Summary, canarySecret) {
		t.Fatalf("SECRET LEAK! Canary secret leaked through model error summary: %s", resF.Summary)
	}
}
