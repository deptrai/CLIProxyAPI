package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestRoutingDebugInSDK(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	mgr := coreauth.NewManager(nil, nil, nil)

	cfg := &internalconfig.Config{
		OAuthModelAlias: map[string][]internalconfig.OAuthModelAlias{
			"devin": {
				{
					Name:         "devin/swe-2(max)",
					Alias:        "claude-fable-5-1",
					Fork:         true,
					ForceMapping: true,
				},
				{
					Name:         "devin/swe-2(high)",
					Alias:        "claude-opus-5",
					Fork:         true,
					ForceMapping: true,
				},
			},
		},
	}
	cfg.SanitizeOAuthModelAlias()
	mgr.SetOAuthModelAlias(cfg.OAuthModelAlias)

	// Auth 1: devin-01 (no local model_aliases)
	auth1 := &coreauth.Auth{
		ID:       "devin-01",
		Provider: "devin",
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}

	// Auth 2: devin-glmfb (with local model_aliases)
	auth2 := &coreauth.Auth{
		ID:       "devin-glmfb",
		Provider: "devin",
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
	}
	coreauth.SetOAuthModelAliasesAttribute(auth2, []internalconfig.OAuthModelAlias{
		{
			Name:         "devin/deepseek-v4-1-flash(max)",
			Alias:        "claude-fable-5-1",
			Fork:         true,
			ForceMapping: true,
		},
	})

	baseModels := registry.GetDevinModels()

	// Apply aliases for auth1
	m1 := applyOAuthModelAliasForAuth(cfg, "devin", "oauth", auth1.Attributes, append([]*registry.ModelInfo(nil), baseModels...))
	// Apply aliases for auth2
	m2 := applyOAuthModelAliasForAuth(cfg, "devin", "oauth", auth2.Attributes, append([]*registry.ModelInfo(nil), baseModels...))

	hasM1Fable := false
	for _, m := range m1 {
		if m.ID == "claude-fable-5-1" {
			hasM1Fable = true
			break
		}
	}
	hasM2Fable := false
	for _, m := range m2 {
		if m.ID == "claude-fable-5-1" {
			hasM2Fable = true
			break
		}
	}

	t.Logf("Auth1 has claude-fable-5-1 in registered models: %v", hasM1Fable)
	t.Logf("Auth2 has claude-fable-5-1 in registered models: %v", hasM2Fable)

	reg.RegisterClient(auth1.ID, "devin", m1)
	reg.RegisterClient(auth2.ID, "devin", m2)

	t.Logf("reg.ClientSupportsModel(auth1, claude-fable-5-1): %v", reg.ClientSupportsModel(auth1.ID, "claude-fable-5-1"))
	t.Logf("reg.ClientSupportsModel(auth2, claude-fable-5-1): %v", reg.ClientSupportsModel(auth2.ID, "claude-fable-5-1"))

	t.Logf("reg.ClientSupportsModel(auth1, devin/swe-2): %v", reg.ClientSupportsModel(auth1.ID, "devin/swe-2"))
	t.Logf("reg.ClientSupportsModel(auth2, devin/deepseek-v4-1-flash): %v", reg.ClientSupportsModel(auth2.ID, "devin/deepseek-v4-1-flash"))
}
