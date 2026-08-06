package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

func Fingerprint(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func DatasetFingerprint(dataset CanonicalDataset) (string, error) {
	type doc struct {
		ID, FileName, MediaType, SHA256 string
		SizeBytes                       int64
		Metadata                        map[string]any
	}
	documents := make([]doc, len(dataset.Corpus))
	for i, item := range dataset.Corpus {
		documents[i] = doc{item.ID, item.FileName, item.MediaType, item.SHA256, item.SizeBytes, item.Metadata}
	}
	cases := append([]Case(nil), dataset.Cases...)
	sort.Slice(documents, func(i, j int) bool { return documents[i].ID < documents[j].ID })
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return Fingerprint(struct {
		Corpus []doc  `json:"corpus"`
		Cases  []Case `json:"cases"`
	}{documents, cases})
}

func ProfileFingerprint(profile Profile) (string, error) {
	if err := profile.Data.Validate(); err != nil {
		return "", fmt.Errorf("invalid profile: %w", err)
	}
	return Fingerprint(profile.Data)
}
