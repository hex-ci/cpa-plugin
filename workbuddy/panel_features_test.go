package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementReturnsEffectiveDesensitizeSettings(t *testing.T) {
	old := featureRuntime.Load()
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [Codex]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(old) })

	path := loadedManagementBasePath() + "/plugins/workbuddy/desensitize"
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   path,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var got struct {
		Enabled bool     `json:"enabled"`
		Terms   []string `json:"terms"`
		Source  string   `json:"source"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Source != "custom" || !reflect.DeepEqual(got.Terms, []string{"Codex"}) {
		t.Fatalf("effective settings = %#v", got)
	}

	if resp := managementResponseForTest(t, pluginapi.ManagementRequest{Method: http.MethodPost, Path: path}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestPanelEditsDesensitizeThroughGenericPluginConfigAPI(t *testing.T) {
	html := string(panelHTML)
	for _, want := range []string{
		`id="desensitizeModal"`,
		`id="desensitizeEnabled"`,
		`id="desensitizeTerms"`,
		`id="desensitizeSource"`,
		`async function managementAPI(path, opts={})`,
		`const MANAGEMENT_BASE_PATH=__WB_MANAGEMENT_BASE_PATH_JSON__`,
		`fetch(MANAGEMENT_BASE_PATH+path`,
		`api("/desensitize")`,
		`managementAPI("/plugins/workbuddy/config",{method:"PATCH"`,
		`JSON.stringify({desensitize_terms:null})`,
		`JSON.stringify({desensitize:false,desensitize_terms:null})`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("panel missing %q", want)
		}
	}
}

func TestPanelSupportsBoundedSequentialFileImport(t *testing.T) {
	html := string(panelHTML)
	for _, want := range []string{
		`id="importFiles"`, `type="file"`, `multiple`, `accept=".json,application/json"`,
		`const MAX_IMPORT_FILE_BYTES=2*1024*1024`, `file.size>MAX_IMPORT_FILE_BYTES`,
		`error:"超过 2 MiB"`, `await file.text()`, `error:"读取失败"`, `if(!imports.length)`,
		`async function importOneCredential(raw)`, `return api("/import"`, `for(const item of imports)`,
		`await importOneCredential(item.raw)`, `const results=[]`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("panel missing %q", want)
		}
	}
	if strings.Contains(html, `localStorage.setItem("workbuddy-import`) {
		t.Fatal("credential import persisted in localStorage")
	}
}

func TestPanelImportSummaryDoesNotIncludeCredentialBearingErrors(t *testing.T) {
	html := string(panelHTML)
	start := strings.Index(html, "async function importAuth(btn){")
	if start < 0 {
		t.Fatal("importAuth function not found")
	}
	end := strings.Index(html[start:], "\ndocument.getElementById(\"keyInput\")")
	if end < 0 {
		t.Fatal("importAuth function end not found")
	}
	body := html[start : start+end]
	for _, forbidden := range []string{`d.error`, `e.message`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("import summary includes server error text %q", forbidden)
		}
	}
}

func TestPanelSearchesAndSortsVisibleAccounts(t *testing.T) {
	html := string(panelHTML)
	for _, want := range []string{
		`id="accountSearch"`, `let accountSearch=""`, `let sortDirection="original"`,
		`function accountsForView(accounts)`, `nickname`, `label`, `name`, `uid`,
		`const view=accountsForFilter(accounts).filter(`, `return view.slice()`,
		`return view.slice().sort(`,
		`sortDirection=sortDirection==="original"?"desc":(sortDirection==="desc"?"asc":"original")`,
		`accountsForView(lastAccounts).map(card).join("")`,
		`const scoped=accountsForFilter(all);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("panel missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`lastAccounts.sort(`,
		`accountsForFilter(lastAccounts).map(card).join("")`,
		`accounts.map(card).join("")`,
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("panel mutates or renders without search/sort: %q", forbidden)
		}
	}
}

func TestPanelWaitsForAsyncConfigReloadBeforeShowingSavedSettings(t *testing.T) {
	html := strings.ReplaceAll(string(panelHTML), "\r\n", "\n")
	for _, want := range []string{
		`async function waitForDesensitizeSettings(matches){`,
		`const d=await api("/desensitize");`,
		`await new Promise(resolve=>setTimeout(resolve,100));`,
		`await waitForDesensitizeSettings(d=>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("panel does not wait for effective runtime settings after async config reload; missing %q", want)
		}
	}

	start := strings.Index(html, "async function saveDesensitize(btn){")
	if start < 0 {
		t.Fatal("saveDesensitize function not found")
	}
	end := strings.Index(html[start:], "\nasync function restoreDesensitizeTerms")
	if end < 0 {
		t.Fatal("saveDesensitize function end not found")
	}
	body := html[start : start+end]
	patch := strings.Index(body, `await managementAPI("/plugins/workbuddy/config"`)
	wait := strings.Index(body, `await waitForDesensitizeSettings(`)
	if patch < 0 || wait < patch {
		t.Fatalf("save does not wait for effective settings after PATCH: patch=%d wait=%d", patch, wait)
	}
}
