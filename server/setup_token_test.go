package server

import "testing"

func TestCheckSetupToken(t *testing.T) {
	s := &Server{setupToken: "setup-ok"}
	if s.checkSetupToken("") || s.checkSetupToken("0000") || s.checkSetupToken("setup-ok-extra") {
		t.Fatal("wrong or empty token was accepted")
	}
	if !s.checkSetupToken("setup-ok") {
		t.Fatal("matching token was rejected")
	}
	s.setupToken = ""
	if s.checkSetupToken("setup-ok") {
		t.Fatal("inactive token was accepted")
	}
}

func TestEnterpriseSkipsSetupToken(t *testing.T) {
	s := &Server{entMode: true, setupToken: "should-not-matter"}
	if !s.enterpriseSkipsSetupToken() {
		t.Fatal("enterprise mode still requires a setup token")
	}
	t.Setenv("ARTEX_ENTERPRISE", "1")
	if !(&Server{}).enterpriseSkipsSetupToken() {
		t.Fatal("ARTEX_ENTERPRISE=1 still requires a setup token before entMode is set")
	}
	t.Setenv("ARTEX_ENTERPRISE", "0")
	if (&Server{}).enterpriseSkipsSetupToken() {
		t.Fatal("standalone mode skipped the setup token")
	}
}
