package main

import (
	"net/http"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/vendorsim/vendorsimtest"
)

func surfaces(t *testing.T) (svc, ctl http.Handler) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a, err := newApp(":8093")
	if err != nil {
		t.Fatal(err)
	}
	vendor, control := a.sim.Surfaces()
	return vendor, control.Handler()
}

func TestIdentityFollowsTheIdentityOutcome(t *testing.T) {
	svc, ctl := surfaces(t)

	code, out := vendorsimtest.Call(t, svc, "POST", "/lexisnexis/oauth/token", `{"client_id":"c","client_secret":"s"}`, nil)
	tok, _ := out["access_token"].(string)
	if code != 200 || tok == "" {
		t.Fatalf("token: %d %v", code, out)
	}
	auth := map[string]string{"Authorization": "Bearer " + tok}

	_, out = vendorsimtest.Call(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{"reference":"r"}`, auth)
	if vendorsimtest.At(out, "data.assessment.result") != "PASS" || vendorsimtest.At(out, "data.attributes.address_verified") != true {
		t.Errorf("IDU by default: %v", out)
	}
	for _, flag := range []string{"sanction", "pep", "insolvency", "ccj", "death_ddri", "death_gro", "death_halo"} {
		if got := vendorsimtest.At(out, "data.attributes."+flag); got != false {
			t.Errorf("IDU %s = %v, want false", flag, got)
		}
	}

	for outcome, want := range map[string]string{"refer": "REFER", "fail": "FAIL"} {
		vendorsimtest.Call(t, ctl, "POST", "/sim/outcome", `{"check":"identity","outcome":"`+outcome+`"}`, nil)
		_, out = vendorsimtest.Call(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{"reference":"r"}`, auth)
		if vendorsimtest.At(out, "data.assessment.result") != want {
			t.Errorf("IDU %s: %v", outcome, out)
		}
		_, out = vendorsimtest.Call(t, svc, "POST", "/lexisnexis/ivi/api/attribute-query", `{"org_id":"o","api_key":"k","policy":"p"}`, nil)
		if vendorsimtest.At(out, "request_result.review_status") != want {
			t.Errorf("IVI %s: %v", outcome, out)
		}
	}
	if code, _ := vendorsimtest.Call(t, svc, "POST", "/lexisnexis/ivi/api/attribute-query", `{"org_id":"o"}`, nil); code != 401 {
		t.Errorf("IVI without its key: %d, want 401", code)
	}
	if code, _ := vendorsimtest.Call(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{}`, nil); code != 401 {
		t.Errorf("IDU without a token: %d, want 401", code)
	}
}

func TestTrapsOnTheTokenAndIVIActInTheEnvironmentTheBodyNames(t *testing.T) {
	routes := map[string][2]string{
		"/lexisnexis/oauth/token":             {`{"client_id":"sbx-user","client_secret":"sbx-secret"}`, `{"client_id":"c","client_secret":"s"}`},
		"/lexisnexis/ivi/api/attribute-query": {`{"org_id":"o","api_key":"sbx-secret","policy":"p"}`, `{"org_id":"o","api_key":"k","policy":"p"}`},
	}
	for path, bodies := range routes {
		sbxBody, defaultBody := bodies[0], bodies[1]
		for _, armedIn := range []string{"sbx", "default"} {
			svc, ctl := surfaces(t)
			vendorsimtest.Call(t, ctl, "PUT", "/sim/customers/sbx", `{"env":"sbx","verification":{"username":"sbx-user","password":"sbx-secret"}}`, nil)
			if code, out := vendorsimtest.Call(t, ctl, "POST", "/sim/traps?env="+armedIn, `{"hook":"http.request","match":{"path":"`+path+`"},"effect":{"respond":{"status":503}},"times":null}`, nil); code != 201 {
				t.Fatalf("arm in %s: %d %v", armedIn, code, out)
			}
			wantSbx, wantDefault := 503, 200
			if armedIn == "default" {
				wantSbx, wantDefault = 200, 503
			}
			if code, _ := vendorsimtest.Call(t, svc, "POST", path, sbxBody, nil); code != wantSbx {
				t.Errorf("%s as sbx, trap in %s: %d, want %d", path, armedIn, code, wantSbx)
			}
			if code, _ := vendorsimtest.Call(t, svc, "POST", path, defaultBody, nil); code != wantDefault {
				t.Errorf("%s as the default, trap in %s: %d, want %d", path, armedIn, code, wantDefault)
			}
		}
	}
}
