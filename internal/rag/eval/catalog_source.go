package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/qs3c/bkcrab/internal/rag/objects"
)

const (
	catalogCachePrefix = "rag-eval/catalog-cache/v1"
	defaultCatalogFile = int64(32 << 20)
	maxCatalogPDF      = int64(128 << 20)
)

var openRAGCorpusPath = regexp.MustCompile(`^pdf/arxiv/corpus/[A-Za-z0-9._-]+\.json$`)
var catalogDownloadGroup singleflight.Group

// CatalogHTTPSource is the only network reader used by built-in evaluation
// dataset adapters. It pins every Hugging Face request to the catalog commit,
// allow-lists logical paths and external hosts, and stores each response in the
// process-wide RAG object store so later imports do not download it again.
type CatalogHTTPSource struct {
	preset CatalogPreset
	store  objects.Store
	client *http.Client
}

func NewCatalogHTTPSource(preset CatalogPreset, objectStore objects.Store, client *http.Client) (*CatalogHTTPSource, error) {
	known, ok := CatalogPresetByID(preset.ID)
	if !ok || preset.Revision != known.Revision || objectStore == nil {
		return nil, errors.New("a pinned built-in catalog preset and object store are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	clone := *client
	previousRedirect := clone.CheckRedirect
	clone.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("catalog download has too many redirects")
		}
		if err := validateCatalogRemoteURL(request.URL); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	return &CatalogHTTPSource{preset: preset, store: objectStore, client: &clone}, nil
}

func (s *CatalogHTTPSource) Open(ctx context.Context, logicalPath string) (io.ReadCloser, error) {
	remote, maxBytes, expectedSHA, err := s.resolve(logicalPath)
	if err != nil {
		return nil, err
	}
	return s.openCached(ctx, "source\x00"+logicalPath, remote, maxBytes, expectedSHA)
}

func (s *CatalogHTTPSource) OpenExternal(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "arxiv.org") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("catalog external URL is not allow-listed")
	}
	cleanPath := path.Clean(parsed.EscapedPath())
	if !strings.HasPrefix(cleanPath, "/pdf/") || strings.Contains(cleanPath, "..") {
		return nil, errors.New("catalog external PDF path is invalid")
	}
	return s.openCached(ctx, "external\x00"+parsed.String(), parsed.String(), maxCatalogPDF, "")
}

func (s *CatalogHTTPSource) List(ctx context.Context, prefix string) ([]CatalogSourceEntry, error) {
	if s.preset.ID != CatalogOpenRAGBench || prefix != "pdf/arxiv/corpus/" {
		return nil, errors.New("catalog directory is not allow-listed")
	}
	remote := fmt.Sprintf("https://huggingface.co/api/datasets/vectara/open_ragbench/tree/%s/pdf/arxiv/corpus?recursive=true&expand=false&limit=1000", s.preset.Revision)
	reader, err := s.openCached(ctx, "list\x00"+prefix, remote, 2<<20, "")
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var raw []struct {
		Type string `json:"type"`
		OID  string `json:"oid"`
		Size int64  `json:"size"`
		Path string `json:"path"`
	}
	decoder := json.NewDecoder(io.LimitReader(reader, (2<<20)+1))
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode catalog directory: %w", err)
	}
	entries := make([]CatalogSourceEntry, 0, len(raw))
	for _, item := range raw {
		if item.Type != "file" || !openRAGCorpusPath.MatchString(item.Path) || item.Size < 0 || item.Size > 16<<20 {
			continue
		}
		entries = append(entries, CatalogSourceEntry{Path: item.Path, Size: item.Size, OID: item.OID})
	}
	if len(entries) == 0 || len(entries) > 1_000 {
		return nil, errors.New("catalog directory contains an unexpected number of files")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (s *CatalogHTTPSource) resolve(logicalPath string) (string, int64, string, error) {
	logicalPath = strings.TrimSpace(logicalPath)
	switch s.preset.ID {
	case CatalogMultiDoc2Dial:
		if logicalPath != "multidoc2dial.zip" {
			return "", 0, "", errors.New("MultiDoc2Dial path is not allow-listed")
		}
		return "https://doc2dial.github.io/multidoc2dial/file/multidoc2dial.zip", 16 << 20, multiDoc2DialArchiveSHA256, nil
	case CatalogTATQA:
		allowed := map[string]struct{}{
			"tatqa_dataset_dev.json": {}, "tatqa_dataset_train.json": {}, "tatqa_dataset_test_gold.json": {},
		}
		if _, ok := allowed[logicalPath]; !ok {
			return "", 0, "", errors.New("TAT-QA path is not allow-listed")
		}
		return s.huggingFaceResolveURL(logicalPath), defaultCatalogFile, "", nil
	case CatalogOpenRAGBench:
		allowed := logicalPath == "pdf/arxiv/queries.json" || logicalPath == "pdf/arxiv/answers.json" || logicalPath == "pdf/arxiv/qrels.json" || logicalPath == "pdf/arxiv/pdf_urls.json" || openRAGCorpusPath.MatchString(logicalPath)
		if !allowed {
			return "", 0, "", errors.New("Open RAGBench path is not allow-listed")
		}
		limit := int64(2 << 20)
		if openRAGCorpusPath.MatchString(logicalPath) {
			limit = 16 << 20
		}
		return s.huggingFaceResolveURL(logicalPath), limit, "", nil
	default:
		return "", 0, "", errors.New("unknown catalog source")
	}
}

func (s *CatalogHTTPSource) huggingFaceResolveURL(logicalPath string) string {
	repository := map[string]string{CatalogTATQA: "next-tat/TAT-QA", CatalogOpenRAGBench: "vectara/open_ragbench"}[s.preset.ID]
	return fmt.Sprintf("https://huggingface.co/datasets/%s/resolve/%s/%s", repository, s.preset.Revision, logicalPath)
}

func (s *CatalogHTTPSource) openCached(ctx context.Context, identity, remote string, maxBytes int64, expectedSHA string) (io.ReadCloser, error) {
	key := s.cacheKey(identity)
	if reader, err := s.store.Get(ctx, key); err == nil {
		return reader, nil
	}
	// The store identity keeps independent stores isolated while still
	// collapsing concurrent imports created from separate source objects.
	downloadKey := fmt.Sprintf("%T:%p:%s", s.store, s.store, key)
	_, err, _ := catalogDownloadGroup.Do(downloadKey, func() (any, error) {
		if reader, getErr := s.store.Get(ctx, key); getErr == nil {
			return nil, reader.Close()
		}
		file, size, mediaType, err := s.download(ctx, remote, maxBytes, expectedSHA)
		if err != nil {
			return nil, err
		}
		name := file.Name()
		defer func() {
			_ = file.Close()
			_ = os.Remove(name)
		}()
		return nil, s.store.Put(ctx, key, file, size, mediaType)
	})
	if err != nil {
		return nil, err
	}
	return s.store.Get(ctx, key)
}

func (s *CatalogHTTPSource) cacheKey(identity string) string {
	sum := sha256.Sum256([]byte(s.preset.ID + "\x00" + s.preset.Revision + "\x00" + identity))
	return catalogCachePrefix + "/" + s.preset.ID + "/" + hex.EncodeToString(sum[:]) + "/blob"
}

func (s *CatalogHTTPSource) download(ctx context.Context, remote string, maxBytes int64, expectedSHA string) (*os.File, int64, string, error) {
	parsed, err := url.Parse(remote)
	if err != nil || validateCatalogRemoteURL(parsed) != nil {
		return nil, 0, "", errors.New("catalog remote URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, 0, "", err
	}
	request.Header.Set("User-Agent", "BkCrab-RAG-Evaluation/1")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, 0, "", fmt.Errorf("download catalog source: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, 0, "", fmt.Errorf("download catalog source: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxBytes {
		return nil, 0, "", errors.New("catalog source exceeds byte limit")
	}
	file, err := os.CreateTemp("", "bkcrab-rag-eval-download-*")
	if err != nil {
		return nil, 0, "", err
	}
	keep := false
	defer func() {
		if !keep {
			name := file.Name()
			_ = file.Close()
			_ = os.Remove(name)
		}
	}()
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, 0, "", err
	}
	if written > maxBytes {
		return nil, 0, "", errors.New("catalog source exceeds byte limit")
	}
	if expectedSHA != "" {
		if hex.EncodeToString(hasher.Sum(nil)) != expectedSHA {
			return nil, 0, "", errors.New("catalog source checksum mismatch")
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, "", err
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	keep = true
	return file, written, mediaType, nil
}

func validateCatalogRemoteURL(parsed *url.URL) error {
	if parsed == nil || parsed.Scheme != "https" || parsed.User != nil {
		return errors.New("catalog remote must be credential-free HTTPS")
	}
	host := strings.ToLower(parsed.Hostname())
	switch host {
	case "huggingface.co", "cdn-lfs.hf.co", "cdn-lfs-us-1.hf.co", "doc2dial.github.io", "arxiv.org", "export.arxiv.org":
		return nil
	default:
		return errors.New("catalog remote host is not allow-listed")
	}
}
