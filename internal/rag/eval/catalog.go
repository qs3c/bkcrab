package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	CatalogMultiDoc2Dial = "ibm-multidoc2dial"
	CatalogTATQA         = "next-tat-tatqa"
	CatalogOpenRAGBench  = "vectara-open-ragbench"
)

type CatalogPreset struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	Description       string         `json:"description"`
	SourceURL         string         `json:"sourceUrl"`
	Revision          string         `json:"revision"`
	License           string         `json:"license"`
	AdapterVersion    string         `json:"adapterVersion"`
	Tracks            []DatasetTrack `json:"tracks"`
	Splits            []string       `json:"splits"`
	EvidenceTypes     []string       `json:"evidenceTypes,omitempty"`
	DefaultSampleSize int            `json:"defaultSampleSize"`
	MaxSampleSize     int            `json:"maxSampleSize"`
	DefaultCorpusSize int            `json:"defaultCorpusSize,omitempty"`
}

var builtinCatalog = []CatalogPreset{
	{
		ID: CatalogMultiDoc2Dial, Name: "MultiDoc2Dial", Description: "多文档、多轮指代检索与回答",
		SourceURL: "https://huggingface.co/datasets/IBM/multidoc2dial", Revision: "1108a969d076f04c7367f0c2427d1c5d6d6bdaa0",
		License: "Apache-2.0", AdapterVersion: "multidoc2dial-v1", Tracks: []DatasetTrack{DatasetTrackTextRAG},
		Splits: []string{"validation", "train", "test"}, DefaultSampleSize: 500, MaxSampleSize: 10_000,
	},
	{
		ID: CatalogTATQA, Name: "TAT-QA", Description: "段落、表格与数值问答",
		SourceURL: "https://huggingface.co/datasets/next-tat/TAT-QA", Revision: "c96247f5077eac447f63527fd3dcfdc58bb56d6a",
		License: "CC-BY-4.0", AdapterVersion: "tatqa-v1", Tracks: []DatasetTrack{DatasetTrackTextRAG},
		Splits: []string{"dev", "train", "test_gold"}, DefaultSampleSize: 1_000, MaxSampleSize: 20_000,
	},
	{
		ID: CatalogOpenRAGBench, Name: "Open RAGBench（Vectara）", Description: "预处理文本主轨与原始 PDF 端到端补充轨",
		SourceURL: "https://huggingface.co/datasets/vectara/open_ragbench", Revision: "63f6b052ff83508b08e242db42263ee708815c26",
		License: "CC-BY-NC-4.0", AdapterVersion: "open-ragbench-arxiv-v1", Tracks: []DatasetTrack{DatasetTrackTextRAG, DatasetTrackPDFE2E},
		Splits: []string{"arxiv"}, EvidenceTypes: []string{"text", "text-table", "text-image", "text-table-image"},
		DefaultSampleSize: 300, MaxSampleSize: 3_045, DefaultCorpusSize: 1_000,
	},
}

func BuiltinCatalog() []CatalogPreset {
	out := make([]CatalogPreset, len(builtinCatalog))
	for index, item := range builtinCatalog {
		out[index] = item
		out[index].Tracks = append([]DatasetTrack(nil), item.Tracks...)
		out[index].Splits = append([]string(nil), item.Splits...)
		out[index].EvidenceTypes = append([]string(nil), item.EvidenceTypes...)
	}
	return out
}

func CatalogPresetByID(id string) (CatalogPreset, bool) {
	for _, item := range builtinCatalog {
		if item.ID == strings.TrimSpace(id) {
			return item, true
		}
	}
	return CatalogPreset{}, false
}

type CatalogImportOptions struct {
	CatalogID     string       `json:"catalogId"`
	Track         DatasetTrack `json:"track"`
	Split         string       `json:"split"`
	SampleSize    int          `json:"sampleSize"`
	Seed          int64        `json:"seed"`
	EvidenceTypes []string     `json:"evidenceTypes,omitempty"`
	CorpusLimit   int          `json:"corpusLimit,omitempty"`
}

func (o *CatalogImportOptions) ApplyDefaults() error {
	preset, ok := CatalogPresetByID(o.CatalogID)
	if !ok {
		return errors.New("unknown built-in evaluation dataset")
	}
	if o.Track == "" {
		o.Track = preset.Tracks[0]
	}
	if !containsTrack(preset.Tracks, o.Track) {
		return errors.New("dataset does not support the selected track")
	}
	if strings.TrimSpace(o.Split) == "" {
		o.Split = preset.Splits[0]
	}
	if !containsString(preset.Splits, o.Split) {
		return errors.New("dataset split is not supported")
	}
	if o.SampleSize == 0 {
		o.SampleSize = preset.DefaultSampleSize
	}
	if o.SampleSize < 1 || o.SampleSize > preset.MaxSampleSize {
		return fmt.Errorf("sample size must be between 1 and %d", preset.MaxSampleSize)
	}
	if o.CorpusLimit < 0 || (o.CatalogID != CatalogOpenRAGBench && o.CorpusLimit != 0) {
		return errors.New("corpus limit is invalid")
	}
	if o.Track == DatasetTrackPDFE2E && o.CorpusLimit == 0 {
		o.CorpusLimit = 50
	}
	if o.Track == DatasetTrackTextRAG && o.CatalogID == CatalogOpenRAGBench && o.CorpusLimit != 0 && o.CorpusLimit != preset.DefaultCorpusSize {
		return errors.New("Open RAGBench text track requires the official complete corpus")
	}
	if len(o.EvidenceTypes) == 0 && o.CatalogID == CatalogOpenRAGBench {
		o.EvidenceTypes = []string{"text"}
	}
	o.EvidenceTypes = uniqueSortedStrings(o.EvidenceTypes)
	for _, evidence := range o.EvidenceTypes {
		if !containsString(preset.EvidenceTypes, evidence) {
			return fmt.Errorf("unsupported evidence type %q", evidence)
		}
	}
	return nil
}

func containsTrack(values []DatasetTrack, wanted DatasetTrack) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// StableSampleIDs ranks IDs by a seeded cryptographic hash. It is independent
// of database/list order and therefore produces a reproducible immutable
// sample on every supported deployment.
func StableSampleIDs(namespace string, ids []string, sampleSize int, seed int64) ([]string, error) {
	if strings.TrimSpace(namespace) == "" || sampleSize < 1 {
		return nil, errors.New("sample namespace and positive size are required")
	}
	ids = uniqueSortedStrings(ids)
	if len(ids) == 0 {
		return nil, errors.New("cannot sample an empty ID set")
	}
	type ranked struct {
		id   string
		hash string
	}
	ranking := make([]ranked, len(ids))
	for index, id := range ids {
		sum := sha256.Sum256([]byte(namespace + "\x00" + strconv.FormatInt(seed, 10) + "\x00" + id))
		ranking[index] = ranked{id: id, hash: hex.EncodeToString(sum[:])}
	}
	sort.Slice(ranking, func(i, j int) bool {
		if ranking[i].hash == ranking[j].hash {
			return ranking[i].id < ranking[j].id
		}
		return ranking[i].hash < ranking[j].hash
	})
	if sampleSize > len(ranking) {
		sampleSize = len(ranking)
	}
	out := make([]string, sampleSize)
	for index := range out {
		out[index] = ranking[index].id
	}
	sort.Strings(out)
	return out, nil
}

type CatalogSourceEntry struct {
	Path string
	Size int64
	OID  string
}

type CatalogSource interface {
	Open(context.Context, string) (io.ReadCloser, error)
	List(context.Context, string) ([]CatalogSourceEntry, error)
	OpenExternal(context.Context, string) (io.ReadCloser, error)
}

type PreparedCatalogDocument struct {
	CorpusDocument
	localPath string
}

func (d PreparedCatalogDocument) Open() (io.ReadCloser, error) {
	return os.Open(d.localPath)
}

type PreparedCatalogDataset struct {
	Dataset   CanonicalDataset
	Documents []PreparedCatalogDocument
	root      string
	mu        sync.Mutex
	rootMu    sync.RWMutex
}

func newPreparedCatalogDataset(name string, track DatasetTrack, source DatasetSource) (*PreparedCatalogDataset, error) {
	root, err := os.MkdirTemp("", "bkcrab-rag-eval-import-*")
	if err != nil {
		return nil, err
	}
	return &PreparedCatalogDataset{Dataset: CanonicalDataset{Name: name, Track: track, Source: source}, root: root}, nil
}

func (p *PreparedCatalogDataset) Close() error {
	if p == nil {
		return nil
	}
	p.rootMu.Lock()
	defer p.rootMu.Unlock()
	if p.root == "" {
		return nil
	}
	root := p.root
	p.root = ""
	return os.RemoveAll(root)
}

func (p *PreparedCatalogDataset) AddDocument(id, fileName, mediaType string, content io.Reader, metadata map[string]any) error {
	if p == nil || !safeObjectID(id) || !safeDatasetFileName(fileName) || content == nil {
		return errors.New("prepared catalog document is invalid")
	}
	p.rootMu.RLock()
	defer p.rootMu.RUnlock()
	root := p.root
	if root == "" {
		return errors.New("prepared catalog document is closed")
	}
	localPath := filepath.Join(root, fileName)
	file, err := os.OpenFile(localPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hasher), content)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(localPath)
		return errors.Join(copyErr, closeErr)
	}
	document := PreparedCatalogDocument{CorpusDocument: CorpusDocument{ID: id, FileName: fileName, MediaType: mediaType,
		SHA256: hex.EncodeToString(hasher.Sum(nil)), SizeBytes: size, Metadata: metadata}, localPath: localPath}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Documents = append(p.Documents, document)
	p.Dataset.Corpus = append(p.Dataset.Corpus, document.CorpusDocument)
	return nil
}

func (p *PreparedCatalogDataset) SortDocuments() {
	p.mu.Lock()
	defer p.mu.Unlock()
	sort.Slice(p.Documents, func(i, j int) bool { return p.Documents[i].ID < p.Documents[j].ID })
	sort.Slice(p.Dataset.Corpus, func(i, j int) bool { return p.Dataset.Corpus[i].ID < p.Dataset.Corpus[j].ID })
}

func catalogDocumentID(prefix, sourceID string) string {
	sum := sha256.Sum256([]byte(sourceID))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

type CatalogAdapter interface {
	ID() string
	Prepare(context.Context, CatalogSource, CatalogImportOptions) (*PreparedCatalogDataset, error)
}

func CatalogAdapterFor(id string) (CatalogAdapter, error) {
	switch strings.TrimSpace(id) {
	case CatalogMultiDoc2Dial:
		return MultiDoc2DialAdapter{}, nil
	case CatalogTATQA:
		return TATQAAdapter{}, nil
	case CatalogOpenRAGBench:
		return OpenRAGBenchAdapter{}, nil
	default:
		return nil, errors.New("unknown built-in evaluation dataset adapter")
	}
}
