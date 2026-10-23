package main

import "testing"

// testAuth builds a credential whose realm and catalogue key are predictable.
func testAuth(realmID, uid string) *storedAuth {
	domain := "https://qoder.com.cn"
	if realmID == realmGlobal {
		domain = "https://qoder.com"
	}
	return &storedAuth{
		Auth:    storedTokens{Domain: domain, AccessToken: "dt-test"},
		Account: storedAccount{UID: uid},
	}
}

// entry mirrors what parseCatalog produces: an unenabled model is Disabled, so
// the publishing path skips it.
func entry(key string, enable bool, factor float64, free bool) catalogEntry {
	return catalogEntry{Key: key, Enable: enable, Disabled: !enable, PriceFactor: factor, IsFree: free}
}

func storeTestCatalog(t *testing.T, sa *storedAuth, entries ...catalogEntry) {
	t.Helper()
	key := catalogKeyFor(sa)
	catalogCache.Store(key, catalogSnapshot{entries: entries, live: true})
	t.Cleanup(func() { catalogCache.Delete(key) })
}

// A free-tier account has zero credits but still serves free models; disabling
// it on "exhausted" takes a working account out of rotation.
func TestLifecycleKeepsFreeModelAccountsEnabled(t *testing.T) {
	// No cache entry: the realm's embedded snapshot is the fallback, and CN's
	// carries the free markers from the live catalogue.
	sa := testAuth(realmCN, "free-cn")
	if !hasEnabledFreeModel(sa) {
		t.Fatal("CN catalog should expose a free model")
	}
	if got := lifecycleActionFor(sa, &creditsSummary{TotalSize: 3000, TotalUsed: 3000}); got != lifecycleNone {
		t.Fatalf("CN action = %v, want none while free models exist", got)
	}
	if !shouldReenable(true, &creditsSummary{TotalSize: 3000, TotalUsed: 3000}, sa) {
		t.Fatal("a disabled account with a free model should be re-enabled")
	}
}

// Without a free model the exhausted account must still be disabled, otherwise
// the lifecycle never stops routing paid traffic to a dead account.
func TestLifecycleDisablesExhaustedPaidAccounts(t *testing.T) {
	pa := testAuth(realmGlobal, "paid-only")
	storeTestCatalog(t, pa,
		entry("paid-a", true, 1.5, false),
		entry("paid-b", false, 0, true),
	)

	if hasEnabledFreeModel(pa) {
		t.Fatal("no enabled free model should be reported")
	}
	if got := lifecycleActionFor(pa, &creditsSummary{TotalSize: 100, TotalUsed: 100}); got != lifecycleDisable {
		t.Fatalf("action = %v, want disable", got)
	}
}

// Entitlements are per credential: one account's catalogue must never decide
// another's, which is how a region ended up advertising only free models.
func TestCatalogIsScopedPerCredentials(t *testing.T) {
	free := testAuth(realmCN, "uid-free")
	paid := testAuth(realmCN, "uid-paid")
	if catalogKeyFor(free) == catalogKeyFor(paid) {
		t.Fatal("catalog keys must differ inside one realm")
	}
	storeTestCatalog(t, free,
		entry("qmodel_38max", false, 0.2, false),
		entry("qfmodel", true, 0, true),
	)
	storeTestCatalog(t, paid,
		entry("qmodel_38max", true, 0.2, false),
		entry("qfmodel", true, 0, true),
	)

	if !hasEnabledFreeModel(free) || !hasEnabledFreeModel(paid) {
		t.Fatal("both accounts expose a free model")
	}
	if got := lifecycleActionFor(paid, &creditsSummary{TotalSize: 100, TotalUsed: 100}); got != lifecycleNone {
		t.Fatalf("paid account action = %v", got)
	}
	// The paid account keeps its paid model even though the free one disabled it.
	merged := mergeCatalogs([][]catalogEntry{mustCatalog(t, free), mustCatalog(t, paid)})
	if len(merged) != 2 {
		t.Fatalf("merged = %#v", merged)
	}
	for _, e := range merged {
		if e.Key == "qmodel_38max" && e.Disabled {
			t.Fatal("a model enabled by any account must stay enabled")
		}
	}
}

// A model disabled by every credential stays disabled.
func TestMergeCatalogsKeepsUniversallyDisabledModels(t *testing.T) {
	a := testAuth(realmGlobal, "g-a")
	b := testAuth(realmGlobal, "g-b")
	storeTestCatalog(t, a, entry("smodel", false, 8, false))
	storeTestCatalog(t, b, entry("smodel", false, 8, false))
	merged := mergeCatalogs([][]catalogEntry{mustCatalog(t, a), mustCatalog(t, b)})
	if len(merged) != 1 || !merged[0].Disabled {
		t.Fatalf("merged = %#v", merged)
	}
}

func mustCatalog(t *testing.T, sa *storedAuth) []catalogEntry {
	t.Helper()
	entries, ok := cachedCatalogState(catalogKeyFor(sa))
	if !ok {
		t.Fatalf("catalog for %s not cached", catalogKeyFor(sa))
	}
	return entries
}
