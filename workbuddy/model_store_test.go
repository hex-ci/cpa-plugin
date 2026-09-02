package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestModelAuthIdentityDoesNotDependOnToken(t *testing.T) {
	left := &storedAuth{Auth: storedTokens{AccessToken: "token-one", Domain: "codebuddy.cn"}, Account: storedAccount{UID: "uid-1", EnterpriseID: "ent-1"}}
	right := &storedAuth{Auth: storedTokens{AccessToken: "token-two", Domain: "codebuddy.cn"}, Account: storedAccount{UID: "uid-1", EnterpriseID: "ent-1"}}
	li, err := modelAuthIdentityFor("auth-a", left)
	if err != nil {
		t.Fatal(err)
	}
	ri, err := modelAuthIdentityFor("auth-b", right)
	if err != nil {
		t.Fatal(err)
	}
	if li.sha256() != ri.sha256() {
		t.Fatalf("token or AuthID changed stable identity: %q != %q", li.sha256(), ri.sha256())
	}
}

func TestModelAuthIdentityUsesTrimmedAuthIDWhenUIDMissing(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "opaque-token", Domain: "codebuddy.cn"},
		Account: storedAccount{EnterpriseID: " enterprise-1 "},
	}
	identity, err := modelAuthIdentityFor("  auth-index-1  ", sa)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Provider != "workbuddy" || identity.Realm != workBuddyRealmCN || identity.UID != "" || identity.EnterpriseID != "enterprise-1" || identity.AuthID != "auth-index-1" {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestModelAuthIdentityHashChangesWithRealmAndEnterprise(t *testing.T) {
	cn := syntheticStoredAuth(t, workBuddyRealmCN)
	global := syntheticStoredAuth(t, workBuddyRealmGlobal)
	cnIdentity, err := modelAuthIdentityFor("auth-1", cn)
	if err != nil {
		t.Fatal(err)
	}
	globalIdentity, err := modelAuthIdentityFor("auth-1", global)
	if err != nil {
		t.Fatal(err)
	}
	if cnIdentity.sha256() == globalIdentity.sha256() {
		t.Fatal("realm change did not change identity hash")
	}

	otherEnterprise := syntheticStoredAuth(t, workBuddyRealmCN)
	otherEnterprise.Account.EnterpriseID = "enterprise-2"
	otherIdentity, err := modelAuthIdentityFor("auth-1", otherEnterprise)
	if err != nil {
		t.Fatal(err)
	}
	if cnIdentity.sha256() == otherIdentity.sha256() {
		t.Fatal("EnterpriseID change did not change identity hash")
	}
}

func TestModelAuthIdentityHashIsLowercaseSHA256Filename(t *testing.T) {
	identity, err := modelAuthIdentityFor("auth-1", syntheticStoredAuth(t, workBuddyRealmCN))
	if err != nil {
		t.Fatal(err)
	}
	if got := identity.sha256(); !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Fatalf("hash = %q, want exactly 64 lowercase hex characters", got)
	}
}

func TestModelStoreSchemaV1RoundTripsAtExactPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "catalog")
	store := newModelStore(root)
	metadata := modelStoreTestMetadata("first")
	hash := strings.Repeat("a", 64)
	models := modelStoreTestCatalog(hash, "first")

	if err := store.saveMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	if err := store.saveModels(models); err != nil {
		t.Fatal(err)
	}

	metadataPath := filepath.Join(root, "metadata.json")
	modelsPath := filepath.Join(root, "models", hash+".json")
	var metadataJSON metadataCacheV1
	if err := json.Unmarshal(modelStoreReadFile(t, metadataPath), &metadataJSON); err != nil {
		t.Fatal(err)
	}
	var modelsJSON modelCatalogCacheV1
	if err := json.Unmarshal(modelStoreReadFile(t, modelsPath), &modelsJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(metadataJSON, metadata) {
		t.Fatalf("metadata round trip = %#v, want %#v", metadataJSON, metadata)
	}
	if !reflect.DeepEqual(modelsJSON, models) {
		t.Fatalf("models round trip = %#v, want %#v", modelsJSON, models)
	}

	loadedMetadata, found, err := store.loadMetadata()
	if err != nil || !found || !reflect.DeepEqual(loadedMetadata, metadata) {
		t.Fatalf("loadMetadata() = %#v, %t, %v", loadedMetadata, found, err)
	}
	loadedModels, found, err := store.loadModels(hash, workBuddyRealmCN)
	if err != nil || !found || !reflect.DeepEqual(loadedModels, models) {
		t.Fatalf("loadModels() = %#v, %t, %v", loadedModels, found, err)
	}
}

func TestModelStoreFirstSaveCreatesPrimaryFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "catalog")
	store := newModelStore(root)
	hash := strings.Repeat("b", 64)
	if err := store.saveMetadata(modelStoreTestMetadata("first")); err != nil {
		t.Fatal(err)
	}
	if err := store.saveModels(modelStoreTestCatalog(hash, "first")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "metadata.json"),
		filepath.Join(root, "models", hash+".json"),
	} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("primary %q: info=%v err=%v", path, info, err)
		}
		if _, err := os.Stat(path + ".bak"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("first save backup %q: err=%v, want not exist", path+".bak", err)
		}
	}
}

func TestModelStoreSecondSaveMovesValidatedPrimaryToBackup(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	first := modelStoreTestMetadata("first")
	second := modelStoreTestMetadata("second")
	if err := store.saveMetadata(first); err != nil {
		t.Fatal(err)
	}
	if err := store.saveMetadata(second); err != nil {
		t.Fatal(err)
	}

	var primary metadataCacheV1
	if err := json.Unmarshal(modelStoreReadFile(t, filepath.Join(root, "metadata.json")), &primary); err != nil {
		t.Fatal(err)
	}
	var backup metadataCacheV1
	if err := json.Unmarshal(modelStoreReadFile(t, filepath.Join(root, "metadata.json.bak")), &backup); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(primary, second) || !reflect.DeepEqual(backup, first) {
		t.Fatalf("primary=%#v backup=%#v", primary, backup)
	}
}

func TestModelStoreLoadsValidBackupWhenPrimaryIsCorrupt(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	first := modelStoreTestMetadata("first")
	if err := store.saveMetadata(first); err != nil {
		t.Fatal(err)
	}
	if err := store.saveMetadata(modelStoreTestMetadata("second")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, found, err := store.loadMetadata()
	if err != nil || !found || !reflect.DeepEqual(got, first) {
		t.Fatalf("loadMetadata() = %#v, %t, %v; want backup %#v", got, found, err, first)
	}
}

func TestModelStoreCorruptPrimaryDoesNotOverwriteValidBackup(t *testing.T) {
	root := t.TempDir()
	store := newModelStore(root)
	first := modelStoreTestMetadata("first")
	if err := store.saveMetadata(first); err != nil {
		t.Fatal(err)
	}
	if err := store.saveMetadata(modelStoreTestMetadata("second")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	third := modelStoreTestMetadata("third")
	if err := store.saveMetadata(third); err != nil {
		t.Fatal(err)
	}

	var backup metadataCacheV1
	if err := json.Unmarshal(modelStoreReadFile(t, filepath.Join(root, "metadata.json.bak")), &backup); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(backup, first) {
		t.Fatalf("backup = %#v, want original last-good %#v", backup, first)
	}
	got, found, err := store.loadMetadata()
	if err != nil || !found || !reflect.DeepEqual(got, third) {
		t.Fatalf("loadMetadata() = %#v, %t, %v", got, found, err)
	}
}

func TestModelStoreCorruptPrimaryAndBackupReturnReadError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.json.bak"), []byte(`also-not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, found, err := newModelStore(root).loadMetadata()
	if err == nil || found {
		t.Fatalf("loadMetadata() = %#v, %t, %v; want no cache and read error", got, found, err)
	}
}

func TestModelStoreMetadataCanonicalValidationMatchesFreshParser(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*modelFacts)
	}{
		{
			name: "empty input modality",
			mutate: func(facts *modelFacts) {
				facts.SupportedInputModalities = []string{""}
			},
		},
		{
			name: "duplicate output modality",
			mutate: func(facts *modelFacts) {
				facts.SupportedOutputModalities = []string{"text", "text"}
			},
		},
		{
			name: "untrimmed input modality",
			mutate: func(facts *modelFacts) {
				facts.SupportedInputModalities = []string{" text "}
			},
		},
		{
			name: "untrimmed canonical name",
			mutate: func(facts *modelFacts) {
				facts.Name = " Model A "
			},
		},
		{
			name: "unsupported canonical description",
			mutate: func(facts *modelFacts) {
				facts.Description = "cache-only description"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+" primary falls through to backup", func(t *testing.T) {
			root := t.TempDir()
			validBackup := modelStoreTestMetadata("valid-backup")
			invalidPrimary := modelStoreTestMetadata("invalid-primary")
			facts := invalidPrimary.Records["vendor/model-a"]
			tt.mutate(&facts)
			invalidPrimary.Records["vendor/model-a"] = facts
			modelStoreWriteJSON(t, filepath.Join(root, "metadata.json"), invalidPrimary)
			modelStoreWriteJSON(t, filepath.Join(root, "metadata.json.bak"), validBackup)

			got, found, err := newModelStore(root).loadMetadata()
			if err != nil || !found || !reflect.DeepEqual(got, validBackup) {
				t.Fatalf("loadMetadata() = %#v, %t, %v; want valid backup %#v", got, found, err, validBackup)
			}
		})

		t.Run(tt.name+" invalid primary and backup", func(t *testing.T) {
			root := t.TempDir()
			for index, name := range []string{"metadata.json", "metadata.json.bak"} {
				invalid := modelStoreTestMetadata(fmt.Sprintf("invalid-%d", index))
				facts := invalid.Records["vendor/model-a"]
				tt.mutate(&facts)
				invalid.Records["vendor/model-a"] = facts
				modelStoreWriteJSON(t, filepath.Join(root, name), invalid)
			}

			got, found, err := newModelStore(root).loadMetadata()
			if err == nil || found {
				t.Fatalf("loadMetadata() = %#v, %t, %v; want no valid canonical cache", got, found, err)
			}
		})
	}
}

func TestModelStoreIdentityMismatchReturnsNoValidCache(t *testing.T) {
	root := t.TempDir()
	requestedHash := strings.Repeat("c", 64)
	otherHash := strings.Repeat("d", 64)
	path := filepath.Join(root, "models", requestedHash+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	modelStoreWriteJSON(t, path, modelStoreTestCatalog(otherHash, "wrong identity"))

	got, found, err := newModelStore(root).loadModels(requestedHash, workBuddyRealmCN)
	if err == nil || found {
		t.Fatalf("loadModels() = %#v, %t, %v; want no valid cache", got, found, err)
	}
}

func TestModelStoreRejectsRealmMismatchedCatalogCaches(t *testing.T) {
	tests := []struct {
		name           string
		expectedRealm  workBuddyRealm
		persistedRealm workBuddyRealm
	}{
		{name: "CN requester with Global cache", expectedRealm: workBuddyRealmCN, persistedRealm: workBuddyRealmGlobal},
		{name: "Global requester with CN cache", expectedRealm: workBuddyRealmGlobal, persistedRealm: workBuddyRealmCN},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			identitySHA256 := strings.Repeat(string(rune('f'-i)), 64)
			path := filepath.Join(root, "models", identitySHA256+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			cache := modelStoreTestCatalog(identitySHA256, "realm-mismatch")
			cache.Realm = tt.persistedRealm
			modelStoreWriteJSON(t, path, cache)

			got, found, err := newModelStore(root).loadModels(identitySHA256, tt.expectedRealm)
			if err == nil || found {
				t.Fatalf("loadModels(%s requester) = %#v, %t, %v; want no valid cache", tt.expectedRealm, got, found, err)
			}
		})
	}
}

func TestModelStoreRealmMismatchPrimaryFallsThroughToMatchingBackup(t *testing.T) {
	root := t.TempDir()
	identitySHA256 := strings.Repeat("a", 64)
	path := filepath.Join(root, "models", identitySHA256+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	matchingBackup := modelStoreTestCatalog(identitySHA256, "matching-backup")
	matchingBackup.Realm = workBuddyRealmCN
	mismatchedPrimary := modelStoreTestCatalog(identitySHA256, "mismatched-primary")
	mismatchedPrimary.Realm = workBuddyRealmGlobal
	modelStoreWriteJSON(t, path, mismatchedPrimary)
	modelStoreWriteJSON(t, path+".bak", matchingBackup)

	got, found, err := newModelStore(root).loadModels(identitySHA256, workBuddyRealmCN)
	if err != nil || !found || !reflect.DeepEqual(got, matchingBackup) {
		t.Fatalf("loadModels(CN requester) = %#v, %t, %v; want matching backup %#v", got, found, err, matchingBackup)
	}
}

func TestModelStoreFutureSchemaReturnsSentinelAndBlocksSave(t *testing.T) {
	t.Run("primary", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "metadata.json")
		future := []byte(`{"schema_version":2,"fetched_at":"2026-08-29T00:00:00Z","records":{"vendor/model-a":{"id":"vendor/model-a"}}}`)
		if err := os.WriteFile(path, future, 0o600); err != nil {
			t.Fatal(err)
		}
		store := newModelStore(root)
		if _, found, err := store.loadMetadata(); found || !errors.Is(err, errFutureModelCacheSchema) {
			t.Fatalf("loadMetadata() found=%t err=%v", found, err)
		}
		if err := store.saveMetadata(modelStoreTestMetadata("replacement")); !errors.Is(err, errFutureModelCacheSchema) {
			t.Fatalf("saveMetadata() err=%v", err)
		}
		if got := modelStoreReadFile(t, path); !bytes.Equal(got, future) {
			t.Fatalf("future primary was overwritten: %s", got)
		}
	})

	t.Run("backup", func(t *testing.T) {
		root := t.TempDir()
		store := newModelStore(root)
		if err := store.saveMetadata(modelStoreTestMetadata("primary")); err != nil {
			t.Fatal(err)
		}
		primaryPath := filepath.Join(root, "metadata.json")
		primary := modelStoreReadFile(t, primaryPath)
		futureBackup := []byte(`{"schema_version":2}`)
		if err := os.WriteFile(primaryPath+".bak", futureBackup, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.saveMetadata(modelStoreTestMetadata("replacement")); !errors.Is(err, errFutureModelCacheSchema) {
			t.Fatalf("saveMetadata() err=%v", err)
		}
		if got := modelStoreReadFile(t, primaryPath); !bytes.Equal(got, primary) {
			t.Fatalf("primary changed despite future backup: %s", got)
		}
		if got := modelStoreReadFile(t, primaryPath+".bak"); !bytes.Equal(got, futureBackup) {
			t.Fatalf("future backup was overwritten: %s", got)
		}
	})
}

func TestModelStoreSavedBytesExcludeAuthSecretsAndAliases(t *testing.T) {
	const (
		accessToken    = "test-access-token-secret"
		refreshToken   = "test-refresh-token-secret"
		authPath       = "C:/private/auth/workbuddy-secret.json"
		rawStorageJSON = `{"private":"raw-storage-secret"}`
		alias          = "private-model-alias"
	)
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: accessToken, RefreshToken: refreshToken, Domain: "codebuddy.cn"},
		Account: storedAccount{EnterpriseID: "enterprise-1"},
	}
	identity, err := modelAuthIdentityFor(strings.Join([]string{authPath, rawStorageJSON, alias}, "|"), sa)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := newModelStore(root)
	if err := store.saveModels(modelStoreTestCatalog(identity.sha256(), "safe model")); err != nil {
		t.Fatal(err)
	}
	raw := modelStoreReadFile(t, filepath.Join(root, "models", identity.sha256()+".json"))
	for _, secret := range []string{accessToken, refreshToken, authPath, rawStorageJSON, alias} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("saved cache contains secret %q: %s", secret, raw)
		}
	}
}

func TestModelStoreUsesPrivateDirectoryAndFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not a Windows ACL guarantee")
	}
	root := filepath.Join(t.TempDir(), "catalog")
	store := newModelStore(root)
	hash := strings.Repeat("e", 64)
	if err := store.saveMetadata(modelStoreTestMetadata("private")); err != nil {
		t.Fatal(err)
	}
	if err := store.saveModels(modelStoreTestCatalog(hash, "private")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "models")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %q mode=%v, want 0700", path, info.Mode().Perm())
		}
	}
	for _, path := range []string{filepath.Join(root, "metadata.json"), filepath.Join(root, "models", hash+".json")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("file %q mode=%v, want 0600", path, info.Mode().Perm())
		}
	}
}

func TestWriteModelCacheAtomicCleansTempsAfterRenameFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metadata.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mutatedDestination := false
	validateCurrent := func(raw []byte) error {
		if !json.Valid(raw) {
			return errors.New("invalid JSON")
		}
		if !mutatedDestination {
			mutatedDestination = true
			if err := os.Remove(path); err != nil {
				return err
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(path, "blocker"), []byte("x"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}
	if err := writeModelCacheAtomic(path, []byte(`{"new":true}`), validateCurrent); err == nil {
		t.Fatal("writeModelCacheAtomic() succeeded despite non-file destination")
	}
	modelStoreAssertNoTemps(t, root)
}

func TestWriteModelCacheAtomicReadOnlyDirectoryFailureLeavesNoTemps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce Unix read-only directory mode bits")
	}
	root := filepath.Join(t.TempDir(), "read-only")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	probe, err := os.CreateTemp(root, ".permission-probe-*")
	if err == nil {
		probePath := probe.Name()
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(probePath); err != nil {
			t.Fatal(err)
		}
		t.Skip("current process can write to a chmod 0500 directory (for example via root or CAP_DAC_OVERRIDE); permission-denial assertion is not applicable")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write probe failed with %v, want permission denied", err)
	}
	if err := writeModelCacheAtomic(filepath.Join(root, "metadata.json"), []byte(`{"new":true}`), func([]byte) error { return nil }); err == nil {
		t.Fatal("writeModelCacheAtomic() succeeded in read-only directory")
	}
	modelStoreAssertNoTemps(t, root)
}

func modelStoreTestMetadata(label string) metadataCacheV1 {
	return metadataCacheV1{
		SchemaVersion: 1,
		ETag:          fmt.Sprintf(`W/"%s"`, label),
		FetchedAt:     time.Date(2026, time.August, 29, 1, 2, 3, 0, time.UTC),
		Records: map[string]modelFacts{
			"vendor/model-a": {ID: "vendor/model-a", Name: "Model A"},
		},
	}
}

func modelStoreTestCatalog(hash, description string) modelCatalogCacheV1 {
	return modelCatalogCacheV1{
		SchemaVersion:  1,
		IdentitySHA256: hash,
		Realm:          workBuddyRealmCN,
		FetchedAt:      time.Date(2026, time.August, 29, 4, 5, 6, 0, time.UTC),
		Endpoint:       workBuddyEndpointV3Config,
		Models:         []modelFacts{{ID: "model-a", Name: "Model A", Description: description}},
	}
}

func modelStoreWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func modelStoreReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func modelStoreAssertNoTemps(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file remains after failure: %q", entry.Name())
		}
	}
}
