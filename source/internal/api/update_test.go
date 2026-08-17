package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/api"
	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/service"
)

// fakeUpdates is an UpdateService the handler tests drive.
type fakeUpdates struct {
	state     domain.UpdateState
	available string
	newer     bool
	checkErr  error
	applyErr  error
	stateErr  error
	applied   []string
}

func (f *fakeUpdates) State(context.Context) (domain.UpdateState, error) {
	return f.state, f.stateErr
}

func (f *fakeUpdates) Check(context.Context) (string, bool, error) {
	return f.available, f.newer, f.checkErr
}

func (f *fakeUpdates) Apply(_ context.Context, version string) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, version)
	f.state = domain.UpdateState{Status: domain.UpdatePending, ToVersion: version}
	return nil
}

func (f *fakeUpdates) ConfirmBoot(context.Context) (bool, error) { return false, nil }
func (f *fakeUpdates) Confirm(context.Context) error             { return nil }

var _ service.UpdateService = (*fakeUpdates)(nil)

// doUpdate drives the router with an UpdateService wired in.
func doUpdate(t *testing.T, updates service.UpdateService, applied func(),
	method, target, body string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, "http://127.0.0.1:8737"+target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tmk_test")
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	api.NewRouter(api.Deps{
		Config:        &fakeConfigService{},
		Providers:     &fakeProviderService{},
		Health:        stubHealth{},
		Auth:          allowAll{},
		Updates:       updates,
		UpdateApplied: applied,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).ServeHTTP(rec, req)
	return rec
}

func TestUpdateStateIsReported(t *testing.T) {
	updates := &fakeUpdates{state: domain.UpdateState{
		Status: domain.UpdatePending, FromVersion: "0.1.0", ToVersion: "0.2.0", BootAttempts: 1,
	}}

	rec := doUpdate(t, updates, nil, http.MethodGet, "/v1/update", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var state domain.UpdateState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, rec.Body)
	}
	if state.Status != domain.UpdatePending || state.ToVersion != "0.2.0" {
		t.Errorf("round trip lost information: %+v", state)
	}
}

// Update state joins /v1/version, because "which version am I running" and "is
// an update half-applied" are the same question after a daemon restarts itself.
func TestVersionCarriesUpdateState(t *testing.T) {
	updates := &fakeUpdates{state: domain.UpdateState{
		Status: domain.UpdatePending, ToVersion: "0.2.0",
	}}

	rec := doUpdate(t, updates, nil, http.MethodGet, "/v1/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"update"`) {
		t.Errorf("version does not carry update state: %s", rec.Body)
	}
}

// A daemon whose update row cannot be read must still answer `version`: it is
// what the updater execs to pre-flight a staged binary, so it has to work on a
// broken install.
func TestVersionSurvivesAnUnreadableUpdateState(t *testing.T) {
	updates := &fakeUpdates{stateErr: errors.New("database is locked")}

	rec := doUpdate(t, updates, nil, http.MethodGet, "/v1/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("version failed because update state could not be read: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"update"`) {
		t.Errorf("an unreadable state was reported anyway: %s", rec.Body)
	}
}

func TestUpdateCheckReportsWhatIsAvailable(t *testing.T) {
	updates := &fakeUpdates{available: "0.2.0", newer: true}

	rec := doUpdate(t, updates, nil, http.MethodPost, "/v1/update/check", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var resp struct {
		Available string `json:"available"`
		Newer     bool   `json:"newer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, rec.Body)
	}
	if resp.Available != "0.2.0" || !resp.Newer {
		t.Errorf("resp = %+v, want 0.2.0/true", resp)
	}
}

// Applying with no body installs whatever is newest, so a client need not know
// the version to ask for.
func TestUpdateApplyWithoutABodyInstallsTheNewest(t *testing.T) {
	updates := &fakeUpdates{available: "0.2.0", newer: true}
	var restarted bool

	rec := doUpdate(t, updates, func() { restarted = true },
		http.MethodPost, "/v1/update/apply", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(updates.applied) != 1 || updates.applied[0] != "0.2.0" {
		t.Errorf("applied %v, want [0.2.0]", updates.applied)
	}
	// The signal fires only AFTER the response is written: an operator who asked
	// for an update and got a dropped connection cannot tell success from a
	// crash.
	if !restarted {
		t.Error("the daemon was not told to restart onto the new binary")
	}
}

func TestUpdateApplyHonoursAnExplicitVersion(t *testing.T) {
	updates := &fakeUpdates{available: "0.3.0", newer: true}

	rec := doUpdate(t, updates, func() {}, http.MethodPost, "/v1/update/apply",
		`{"version":"0.2.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(updates.applied) != 1 || updates.applied[0] != "0.2.0" {
		t.Errorf("applied %v, want the version asked for", updates.applied)
	}
}

// Already up to date is a CONFLICT, not a success: a client that asked to
// update and got 200 would reasonably expect a restart.
func TestUpdateApplyWhenAlreadyUpToDate(t *testing.T) {
	updates := &fakeUpdates{available: "0.1.0", newer: false}
	var restarted bool

	rec := doUpdate(t, updates, func() { restarted = true },
		http.MethodPost, "/v1/update/apply", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	if restarted {
		t.Error("the daemon was told to restart without installing anything")
	}
}

// A failed apply must not signal a restart — relaunching onto a binary that was
// never installed is how a daemon stops coming back.
func TestUpdateApplyFailureDoesNotRestart(t *testing.T) {
	updates := &fakeUpdates{
		available: "0.2.0", newer: true,
		applyErr: errors.New("checksum mismatch"),
	}
	var restarted bool

	rec := doUpdate(t, updates, func() { restarted = true },
		http.MethodPost, "/v1/update/apply", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("a failed apply reported success: %s", rec.Body)
	}
	if restarted {
		t.Error("a failed apply signalled a restart")
	}
}

// Without an UpdateService the endpoints say WHY rather than 404, which reads
// as "wrong URL" instead of "not how you update this deployment".
func TestUpdateEndpointsWhenSelfUpdateIsDisabled(t *testing.T) {
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/v1/update"},
		{http.MethodPost, "/v1/update/check"},
		{http.MethodPost, "/v1/update/apply"},
	} {
		rec := doUpdate(t, nil, nil, tc.method, tc.target, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400", tc.method, tc.target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "update_unsupported") {
			t.Errorf("%s %s: body = %s", tc.method, tc.target, rec.Body)
		}
	}
}

// And /v1/version still works with no updater — it is the endpoint that must
// answer on any install.
func TestVersionWorksWithoutAnUpdateService(t *testing.T) {
	rec := doUpdate(t, nil, nil, http.MethodGet, "/v1/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"update"`) {
		t.Errorf("update state was reported with no updater: %s", rec.Body)
	}
}

func TestUpdateApplyRejectsAMalformedBody(t *testing.T) {
	for _, body := range []string{`{`, `{"version":"0.2.0","unknown":1}`} {
		updates := &fakeUpdates{available: "0.2.0", newer: true}

		rec := doUpdate(t, updates, func() {}, http.MethodPost, "/v1/update/apply", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
		if len(updates.applied) != 0 {
			t.Errorf("body %q reached the service", body)
		}
	}
}

// A failure to read the row is reported rather than answered with an empty
// state, which a client would read as "nothing happening".
func TestUpdateStateReportsAFailure(t *testing.T) {
	updates := &fakeUpdates{stateErr: errors.New("database is locked")}

	rec := doUpdate(t, updates, nil, http.MethodGet, "/v1/update", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("an unreadable state answered 200: %s", rec.Body)
	}
}

// A failed check is a 502, not a 500: GitHub being unreachable is not tumika
// failing, and a 500 sends the operator to look at the wrong system.
func TestUpdateCheckReportsAnUnreachableSource(t *testing.T) {
	updates := &fakeUpdates{checkErr: errors.New("connection reset")}

	rec := doUpdate(t, updates, nil, http.MethodPost, "/v1/update/check", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("a failed check answered 200: %s", rec.Body)
	}
}

// Apply with an explicit version must not need a check to succeed first — an
// operator pinning a version has already decided.
func TestUpdateApplyWithAnExplicitVersionSkipsTheCheck(t *testing.T) {
	updates := &fakeUpdates{checkErr: errors.New("connection reset")}

	rec := doUpdate(t, updates, func() {}, http.MethodPost, "/v1/update/apply",
		`{"version":"0.2.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite the check failing: %s", rec.Code, rec.Body)
	}
	if len(updates.applied) != 1 {
		t.Errorf("applied %v", updates.applied)
	}
}

// Apply without a version DOES need the check, and reports its failure.
func TestUpdateApplyWithoutAVersionNeedsTheCheck(t *testing.T) {
	updates := &fakeUpdates{checkErr: errors.New("connection reset")}

	rec := doUpdate(t, updates, func() {}, http.MethodPost, "/v1/update/apply", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("a failed check answered 200: %s", rec.Body)
	}
	if len(updates.applied) != 0 {
		t.Error("something was installed despite the check failing")
	}
}
