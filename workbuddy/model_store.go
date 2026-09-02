package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const modelCacheSchemaVersion = 1

var errFutureModelCacheSchema = errors.New("future model cache schema")

type modelAuthIdentity struct {
	Provider     string         `json:"provider"`
	Realm        workBuddyRealm `json:"realm"`
	UID          string         `json:"uid,omitempty"`
	EnterpriseID string         `json:"enterprise_id,omitempty"`
	AuthID       string         `json:"auth_id,omitempty"`
}

type metadataCacheV1 struct {
	SchemaVersion int                   `json:"schema_version"`
	ETag          string                `json:"etag,omitempty"`
	FetchedAt     time.Time             `json:"fetched_at"`
	Records       map[string]modelFacts `json:"records"`
}

type modelCatalogCacheV1 struct {
	SchemaVersion  int                   `json:"schema_version"`
	IdentitySHA256 string                `json:"identity_sha256"`
	Realm          workBuddyRealm        `json:"realm"`
	FetchedAt      time.Time             `json:"fetched_at"`
	Endpoint       workBuddyEndpointKind `json:"endpoint"`
	Models         []modelFacts          `json:"models"`
}

type modelStore struct {
	root string
}

func modelAuthIdentityFor(authID string, sa *storedAuth) (modelAuthIdentity, error) {
	if sa == nil {
		return modelAuthIdentity{}, fmt.Errorf("stored auth is nil")
	}
	realm, err := workBuddyRealmFromAccessToken(sa.Auth.AccessToken)
	if err != nil {
		realm = workBuddyRealm(accountRegion(sa))
	}
	identity := modelAuthIdentity{
		Provider:     providerName,
		Realm:        realm,
		UID:          strings.TrimSpace(sa.Account.UID),
		EnterpriseID: strings.TrimSpace(sa.Account.EnterpriseID),
	}
	if identity.UID == "" {
		identity.AuthID = strings.TrimSpace(authID)
		if identity.AuthID == "" {
			return modelAuthIdentity{}, fmt.Errorf("model auth identity has neither UID nor AuthID")
		}
	}
	return identity, nil
}

func (i modelAuthIdentity) sha256() string {
	i.Provider = strings.TrimSpace(i.Provider)
	i.UID = strings.TrimSpace(i.UID)
	i.EnterpriseID = strings.TrimSpace(i.EnterpriseID)
	if i.UID == "" {
		i.AuthID = strings.TrimSpace(i.AuthID)
	} else {
		i.AuthID = ""
	}
	raw, _ := json.Marshal(i)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func defaultModelStore() (*modelStore, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(configDir, "CLIProxyAPI", "workbuddy", "model-catalog")
	return newModelStore(root), nil
}

func newModelStore(root string) *modelStore {
	return &modelStore{root: root}
}

func (s *modelStore) loadMetadata() (metadataCacheV1, bool, error) {
	return loadModelCache(filepath.Join(s.root, "metadata.json"), decodeMetadataCache)
}

func (s *modelStore) saveMetadata(cache metadataCacheV1) error {
	if err := validateMetadataCache(cache); err != nil {
		return err
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	return writeModelCacheAtomic(filepath.Join(s.root, "metadata.json"), raw, func(current []byte) error {
		_, err := decodeMetadataCache(current)
		return err
	})
}

func (s *modelStore) loadModels(identitySHA256 string, expectedRealm workBuddyRealm) (modelCatalogCacheV1, bool, error) {
	if !validModelIdentitySHA256(identitySHA256) {
		return modelCatalogCacheV1{}, false, fmt.Errorf("model cache identity hash is invalid")
	}
	path := filepath.Join(s.root, "models", identitySHA256+".json")
	return loadModelCache(path, func(raw []byte) (modelCatalogCacheV1, error) {
		return decodeModelCatalogCache(raw, identitySHA256, expectedRealm)
	})
}

func (s *modelStore) saveModels(cache modelCatalogCacheV1) error {
	if err := validateModelCatalogCache(cache, cache.IdentitySHA256, cache.Realm); err != nil {
		return err
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	path := filepath.Join(s.root, "models", cache.IdentitySHA256+".json")
	return writeModelCacheAtomic(path, raw, func(current []byte) error {
		_, err := decodeModelCatalogCache(current, cache.IdentitySHA256, cache.Realm)
		return err
	})
}

func loadModelCache[T any](path string, decode func([]byte) (T, error)) (T, bool, error) {
	var zero T
	var readErrors []error
	foundFile := false
	for _, candidate := range []string{path, path + ".bak"} {
		raw, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		foundFile = true
		if err != nil {
			readErrors = append(readErrors, fmt.Errorf("read model cache: %w", err))
			continue
		}
		cache, err := decode(raw)
		if err == nil {
			return cache, true, nil
		}
		if errors.Is(err, errFutureModelCacheSchema) {
			return zero, false, errFutureModelCacheSchema
		}
		readErrors = append(readErrors, fmt.Errorf("read model cache: %w", err))
	}
	if !foundFile {
		return zero, false, nil
	}
	return zero, false, errors.Join(readErrors...)
}

func decodeMetadataCache(raw []byte) (metadataCacheV1, error) {
	var cache metadataCacheV1
	if err := json.Unmarshal(raw, &cache); err != nil {
		return metadataCacheV1{}, fmt.Errorf("decode metadata cache: %w", err)
	}
	if err := validateMetadataCache(cache); err != nil {
		return metadataCacheV1{}, err
	}
	return cache, nil
}

func validateMetadataCache(cache metadataCacheV1) error {
	if err := validateModelCacheSchema(cache.SchemaVersion); err != nil {
		return err
	}
	if cache.FetchedAt.IsZero() {
		return fmt.Errorf("metadata cache fetched_at is missing")
	}
	if len(cache.Records) == 0 {
		return fmt.Errorf("metadata cache records are empty")
	}
	for canonicalID, facts := range cache.Records {
		validated, err := validateModelsDevCanonicalRecord(canonicalID, facts)
		if err != nil {
			return fmt.Errorf("metadata cache record is invalid: %w", err)
		}
		if validated.Name != facts.Name ||
			!slices.Equal(validated.SupportedInputModalities, facts.SupportedInputModalities) ||
			!slices.Equal(validated.SupportedOutputModalities, facts.SupportedOutputModalities) {
			return fmt.Errorf("metadata cache record is not normalized")
		}
	}
	return nil
}

func decodeModelCatalogCache(raw []byte, identitySHA256 string, expectedRealm workBuddyRealm) (modelCatalogCacheV1, error) {
	var cache modelCatalogCacheV1
	if err := json.Unmarshal(raw, &cache); err != nil {
		return modelCatalogCacheV1{}, fmt.Errorf("decode model catalog cache: %w", err)
	}
	if err := validateModelCatalogCache(cache, identitySHA256, expectedRealm); err != nil {
		return modelCatalogCacheV1{}, err
	}
	return cache, nil
}

func validateModelCatalogCache(cache modelCatalogCacheV1, identitySHA256 string, expectedRealm workBuddyRealm) error {
	if err := validateModelCacheSchema(cache.SchemaVersion); err != nil {
		return err
	}
	if !validModelIdentitySHA256(cache.IdentitySHA256) || cache.IdentitySHA256 != identitySHA256 {
		return fmt.Errorf("model cache identity does not match")
	}
	if expectedRealm != workBuddyRealmCN && expectedRealm != workBuddyRealmGlobal {
		return fmt.Errorf("expected model cache realm is invalid")
	}
	if cache.Realm != expectedRealm {
		return fmt.Errorf("model cache realm does not match")
	}
	if cache.FetchedAt.IsZero() {
		return fmt.Errorf("model cache fetched_at is missing")
	}
	if cache.Endpoint != workBuddyEndpointV3Config && cache.Endpoint != workBuddyEndpointLegacyPersonalModels {
		return fmt.Errorf("model cache endpoint is invalid")
	}
	validated, err := validateModelFacts(cache.Models)
	if err != nil {
		return fmt.Errorf("model cache models are invalid: %w", err)
	}
	for i := range validated {
		if validated[i].ID != cache.Models[i].ID || validated[i].Name != cache.Models[i].Name {
			return fmt.Errorf("model cache models are not normalized")
		}
	}
	return nil
}

func validateModelCacheSchema(version int) error {
	if version > modelCacheSchemaVersion {
		return errFutureModelCacheSchema
	}
	if version != modelCacheSchemaVersion {
		return fmt.Errorf("model cache schema version is invalid")
	}
	return nil
}

func validModelIdentitySHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func writeModelCacheAtomic(path string, data []byte, validateCurrent func([]byte) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	primaryTemp, err := writeSyncedModelCacheTemp(dir, filepath.Base(path), data)
	if err != nil {
		return err
	}
	defer os.Remove(primaryTemp)

	current, currentExists, err := readModelCacheIfExists(path)
	if err != nil {
		return err
	}
	backup, backupExists, err := readModelCacheIfExists(path + ".bak")
	if err != nil {
		return err
	}

	currentValid := false
	if currentExists {
		err = validateCurrent(current)
		if errors.Is(err, errFutureModelCacheSchema) {
			return errFutureModelCacheSchema
		}
		currentValid = err == nil
	}
	if backupExists {
		if err := validateCurrent(backup); errors.Is(err, errFutureModelCacheSchema) {
			return errFutureModelCacheSchema
		}
	}

	if currentValid {
		backupTemp, err := writeSyncedModelCacheTemp(dir, filepath.Base(path)+".bak", current)
		if err != nil {
			return err
		}
		defer os.Remove(backupTemp)
		if err := replaceModelCacheFile(backupTemp, path+".bak"); err != nil {
			return err
		}
	}
	if err := replaceModelCacheFile(primaryTemp, path); err != nil {
		return err
	}
	return syncModelCacheDirectory(dir)
}

func readModelCacheIfExists(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func writeSyncedModelCacheTemp(dir, base string, data []byte) (string, error) {
	file, err := os.CreateTemp(dir, "."+base+".tmp-")
	if err != nil {
		return "", err
	}
	path := file.Name()
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	n, err := file.Write(data)
	if err != nil {
		return "", err
	}
	if n != len(data) {
		return "", fmt.Errorf("write model cache temp: wrote %d of %d bytes", n, len(data))
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	keep = true
	return path, nil
}

func replaceModelCacheFile(temp, destination string) error {
	if runtime.GOOS == "windows" {
		if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(temp, destination)
}

func syncModelCacheDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
