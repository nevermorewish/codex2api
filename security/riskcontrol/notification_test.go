package riskcontrol

import "testing"

func TestRiskControlRetiredScopeAndEmailSettings(t *testing.T) {
	c := DefaultConfig()
	c.ModelFilter = "include"
	c.Models = []string{"one-model"}
	c.GroupIDs = []int64{7}
	c.APIKeyIDs = []int64{9}
	c.EmailOnHit = true
	c.SMTPHost = "legacy-host"
	c.SMTPPassword = "legacy-secret"
	c.EmailTo = "invalid-legacy-address"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.ModelFilter != "all" || len(c.Models)+len(c.GroupIDs)+len(c.APIKeyIDs) != 0 {
		t.Fatal("hidden scope restriction survived")
	}
	if c.EmailOnHit || c.SMTPHost != "" || c.SMTPPassword != "" || c.EmailTo != "" {
		t.Fatal("removed mail settings survived")
	}
}
